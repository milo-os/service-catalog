// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
	"go.miloapis.com/services/internal/config"
	"go.miloapis.com/services/internal/controller"
	serviceswebhooks "go.miloapis.com/services/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	codecs   = serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	// Build metadata, set via -ldflags at build time. See Dockerfile.
	version      = "dev"
	gitCommit    = "unknown"
	gitTreeState = "unknown"
	buildDate    = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(config.RegisterDefaults(scheme))
	utilruntime.Must(servicesv1alpha1.AddToScheme(scheme))
	utilruntime.Must(billingv1alpha1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	var enableLeaderElection bool
	var leaderElectionNamespace string
	var probeAddr string
	var serverConfigFile string

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-elect-namespace", "", "The namespace to use for leader election.")
	flag.StringVar(&serverConfigFile, "server-config", "", "path to the server config file")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info("starting services",
		"version", version,
		"gitCommit", gitCommit,
		"gitTreeState", gitTreeState,
		"buildDate", buildDate,
	)

	var serverConfig config.ServicesOperator
	var configData []byte
	if len(serverConfigFile) > 0 {
		var err error
		configData, err = os.ReadFile(serverConfigFile)
		if err != nil {
			setupLog.Error(fmt.Errorf("unable to read server config from %q", serverConfigFile), "")
			os.Exit(1)
		}
	}

	if err := runtime.DecodeInto(codecs.UniversalDecoder(), configData, &serverConfig); err != nil {
		setupLog.Error(err, "unable to decode server config")
		os.Exit(1)
	}

	setupLog.Info("server config", "config", serverConfig)

	// Services resources live in the Milo control plane, not in the cluster
	// that hosts the controller pod. Connect directly to Milo using the
	// configured kubeconfig, falling back to ctrl.GetConfig() for local /
	// in-cluster development where they happen to be the same cluster.
	cfg, err := serverConfig.RestConfig()
	if err != nil {
		setupLog.Error(err, "unable to load rest config")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	// Build a direct (non-cached) client so the metrics and webhook TLS
	// option builders have a Secret-capable client available before the
	// manager (and its cache) have started. The client is invoked lazily
	// from within TLS GetCertificate callbacks, so reads happen after the
	// manager is running.
	bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create bootstrap client")
		os.Exit(1)
	}

	metricsServerOptions := serverConfig.MetricsServer.Options(ctx, bootstrapClient)

	var webhookServer webhook.Server
	if serverConfig.WebhookServer != nil {
		webhookServer = webhook.NewServer(
			serverConfig.WebhookServer.Options(ctx, bootstrapClient),
		)
	} else {
		setupLog.Info("webhookServer not configured; admission webhook server disabled")
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsServerOptions,
		WebhookServer:           webhookServer,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "services.miloapis.com",
		LeaderElectionNamespace: leaderElectionNamespace,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.ServiceReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Service")
		os.Exit(1)
	}
	if err = (&controller.ServiceConfigurationReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceConfiguration")
		os.Exit(1)
	}

	if serverConfig.WebhookServer != nil {
		if err = serviceswebhooks.SetupServiceWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Service")
			os.Exit(1)
		}
		if err = serviceswebhooks.SetupServiceConfigurationWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ServiceConfiguration")
			os.Exit(1)
		}
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
}
