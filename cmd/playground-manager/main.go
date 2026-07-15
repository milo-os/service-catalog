// SPDX-License-Identifier: AGPL-3.0-only

// playground-manager runs the service-catalog reconcilers for the real
// environment playground against the single shared kind cluster.
//
// Why a dedicated entrypoint rather than cmd/services: the production manager
// wires the Milo multicluster provider, which needs real Project virtual
// control planes. The playground has one kind cluster and no Milo platform, so
// — exactly like the agent-framework e2e harness — the multicluster manager
// gets a static provider: one cluster.Cluster (the kind apiserver, via the
// in-cluster ServiceAccount) engaged under the consumer project name, and
// resolvable under the provider project alias. Every catalog kind is
// cluster-scoped, so this single cluster faithfully models one consumer
// project's control plane.
//
// It wires the reconcilers the e2e leg proved end-to-end (Service,
// ServiceEntitlement, ServiceAgent, ServiceAgentConfiguration, AgentBinding)
// and, unlike the e2e harness, optionally serves the admission webhooks
// through the cluster's cert-manager (--enable-webhooks), so `kubectl apply`
// of a malformed agent configuration is rejected the same way it is in
// production.
package main

import (
	"flag"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/controller"
	serviceswebhooks "go.miloapis.com/service-catalog/internal/webhook/v1alpha1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("playground-setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(servicesv1alpha1.AddToScheme(scheme))
}

func main() {
	var consumerProject string
	var aliases stringSlice
	var enableWebhooks bool
	var probeAddr string
	flag.StringVar(&consumerProject, "consumer-project", "demo-project",
		"cluster name the kind cluster is engaged under (the consumer project)")
	flag.Var(&aliases, "alias",
		"additional cluster names resolving to the same cluster, e.g. the provider project (repeatable)")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", false,
		"serve admission webhooks (requires a cert-manager serving cert mounted at the controller-runtime default path)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if len(aliases) == 0 {
		// Default provider project; must match the StreamCo Service sample's
		// owner.producerProjectRef.
		aliases = stringSlice{"streamco-platform"}
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to load kubeconfig (expects in-cluster config or KUBECONFIG)")
		os.Exit(1)
	}

	// Primary manager: platform-scoped reconcilers and the webhook server run
	// against the root (and only) cluster.
	managerOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddr,
	}
	mgr, err := ctrl.NewManager(cfg, managerOptions)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.ServiceReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Service")
		os.Exit(1)
	}
	if err := (&controller.ServiceAgentReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceAgent")
		os.Exit(1)
	}
	if err := (&controller.ServiceAgentConfigurationReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceAgentConfiguration")
		os.Exit(1)
	}

	// Multicluster manager with the static single-cluster provider:
	// project-scoped reconcilers (ServiceEntitlement, AgentBinding).
	projectCluster, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		setupLog.Error(err, "unable to create project cluster")
		os.Exit(1)
	}
	provider := newStaticProvider(consumerProject, projectCluster, aliases...)

	mcMgr, err := mcmanager.New(cfg, provider, mcmanager.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		setupLog.Error(err, "unable to create multicluster manager")
		os.Exit(1)
	}

	if err := (&controller.ServiceEntitlementReconciler{Scheme: scheme}).SetupWithManager(mcMgr, mgr.GetClient()); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceEntitlement")
		os.Exit(1)
	}
	if err := (&controller.AgentBindingReconciler{Scheme: scheme}).SetupWithManager(mcMgr, mgr.GetClient()); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentBinding")
		os.Exit(1)
	}

	if enableWebhooks {
		if err := setupWebhooks(mgr); err != nil {
			setupLog.Error(err, "unable to set up admission webhooks")
			os.Exit(1)
		}
		setupLog.Info("admission webhooks enabled")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	// The static provider and both managers block; run the auxiliaries in
	// goroutines, mirroring cmd/services/main.go. The local "" cluster is
	// deliberately NOT engaged — reconcilers reject empty cluster names and
	// all project-scoped watches come from the static provider.
	go func() {
		if err := provider.Run(ctx, mcMgr); err != nil {
			setupLog.Error(err, "static provider failed")
			os.Exit(1)
		}
	}()
	go func() {
		if err := mcMgr.Start(ctx); err != nil {
			setupLog.Error(err, "multicluster manager failed")
			os.Exit(1)
		}
	}()

	setupLog.Info("PLAYGROUND_MANAGER_STARTING",
		"consumerProject", consumerProject, "aliases", aliases.String(), "webhooks", enableWebhooks)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager failed")
		os.Exit(1)
	}
}

// setupWebhooks registers every services.miloapis.com admission webhook on the
// primary manager. Paths match config/base/webhook/manifests.yaml, which the
// playground overlay wires to the cert-manager-issued serving cert.
func setupWebhooks(mgr ctrl.Manager) error {
	if err := serviceswebhooks.SetupServiceWebhookWithManager(mgr); err != nil {
		return err
	}
	if err := serviceswebhooks.SetupServiceConfigurationWebhookWithManager(mgr); err != nil {
		return err
	}
	if err := serviceswebhooks.SetupServiceEntitlementWebhookWithManager(mgr); err != nil {
		return err
	}
	if err := serviceswebhooks.SetupServiceConsumerWebhookWithManager(mgr); err != nil {
		return err
	}
	if err := serviceswebhooks.SetupServiceAgentWebhookWithManager(mgr); err != nil {
		return err
	}
	return serviceswebhooks.SetupServiceAgentConfigurationWebhookWithManager(mgr)
}

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
