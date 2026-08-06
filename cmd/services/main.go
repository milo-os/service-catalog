// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"
	milowebhook "go.miloapis.com/milo/pkg/webhook"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/config"
	"go.miloapis.com/service-catalog/internal/controller"
	serviceswebhooks "go.miloapis.com/service-catalog/internal/webhook/v1alpha1"
	consumer "go.miloapis.com/service-catalog/pkg/multicluster-runtime/consumer"
	"go.miloapis.com/service-catalog/pkg/multicluster-runtime/e2esingle"
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
	utilruntime.Must(quotav1alpha1.AddToScheme(scheme))
	utilruntime.Must(resourcemanagerv1alpha1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	var enableLeaderElection bool
	var leaderElectionNamespace string
	var probeAddr string
	var serverConfigFile string
	var enableSingleClusterForE2ETests bool

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-elect-namespace", "", "The namespace to use for leader election.")
	flag.StringVar(&serverConfigFile, "server-config", "", "path to the server config file")
	flag.BoolVar(&enableSingleClusterForE2ETests, "enable-single-cluster-for-e2e-tests", false,
		"DANGEROUS outside e2e: replaces the Milo multicluster provider with one that resolves every "+
			"project name (consumer and provider alike) to this single cluster, so ServiceEntitlement/"+
			"ServiceConsumer/ProjectSuspensionPropagation can be exercised without a real Milo control "+
			"plane. Collapses project isolation entirely — must never be set outside the e2e chainsaw suite.")

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
		// Wrap with the cluster-aware server so admission handlers can resolve
		// which project control plane a request targets (via the
		// iam.miloapis.com/parent-name extra) and authorize the caller there.
		webhookServer = milowebhook.NewClusterAwareServer(webhook.NewServer(
			serverConfig.WebhookServer.Options(ctx, bootstrapClient),
		))
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
	if err = (&controller.ServiceAvailabilityReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceAvailability")
		os.Exit(1)
	}
	if err = (&controller.OrganizationDefaultsReconciler{Scheme: scheme}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OrganizationDefaults")
		os.Exit(1)
	}
	if err = (&controller.BillingEntitlementDefaultsReconciler{Scheme: scheme}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BillingEntitlementDefaults")
		os.Exit(1)
	}

	// ServiceEntitlement and ServiceConsumer live in project virtual control
	// planes. We need a multicluster manager backed by the Milo provider so
	// reconcilers run inside every engaged project's cluster context.
	// The provider runs on its own manager rather than on mgr, so that
	// project discovery and cluster engagement are not leader-gated; see
	// newDiscoveryManager. It doubles as the multicluster manager's local
	// manager.
	discoveryMgr, err := newDiscoveryManager(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create project discovery manager")
		os.Exit(1)
	}

	var provider mcProvider
	if enableSingleClusterForE2ETests {
		setupLog.Info("enable-single-cluster-for-e2e-tests is set: using e2esingle provider, " +
			"NOT the Milo multicluster provider — project isolation is disabled")
		provider = e2esingle.New(mgr)
	} else {
		miloProvider, err := miloprovider.New(discoveryMgr, miloprovider.Options{
			InternalServiceDiscovery: false,
			ProjectRestConfig:        cfg,
			// Engaged project clusters must use our scheme; without it their cache
			// falls back to the client-go global scheme, which lacks the
			// services.miloapis.com types, and every ServiceEntitlement /
			// ServiceConsumer / LocationBinding watch fails with "kind must be
			// registered to the Scheme".
			ClusterOptions: []cluster.Option{
				func(o *cluster.Options) { o.Scheme = scheme },
			},
		})
		if err != nil {
			setupLog.Error(err, "unable to create Milo multicluster provider")
			os.Exit(1)
		}
		provider = miloProvider
	}

	mcMgr, err := newMulticlusterManager(discoveryMgr, provider)
	if err != nil {
		setupLog.Error(err, "unable to create multicluster manager")
		os.Exit(1)
	}

	if err = (&controller.ServiceEntitlementReconciler{Scheme: scheme}).SetupWithManager(mcMgr, mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceEntitlement")
		os.Exit(1)
	}
	if err = (&controller.ServiceConsumerReconciler{Scheme: scheme}).SetupWithManager(mcMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceConsumer")
		os.Exit(1)
	}
	if err = (&controller.ProjectSuspensionPropagationReconciler{Scheme: scheme}).SetupWithManager(mgr, mcMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ProjectSuspensionPropagation")
		os.Exit(1)
	}
	// LocationBinding projection runs either on the all-projects manager
	// (today's default) or, when consumer-scoped projection is enabled, on a
	// dedicated multicluster manager whose membership is driven by the consumer
	// provider — only consumer projects with an active ServiceConsumer for one
	// of the configured services. The reconciler is unchanged in either case;
	// only which manager hosts it differs. Its rootClient stays pinned to the
	// all-projects manager's client, where the cluster-scoped
	// ServiceConfiguration / ServiceAvailability / Location objects live.
	if csp := serverConfig.ConsumerScopedProjection; csp != nil {
		if csp.ProviderProject == "" {
			setupLog.Error(fmt.Errorf("consumerScopedProjection.providerProject must be set"),
				"invalid consumer-scoped projection config")
			os.Exit(1)
		}

		// The provider project hosts this operator's ServiceConsumer objects.
		// Build a manager pointed at its control plane; the consumer provider
		// reads the provider project from this manager's rest.Config and
		// re-addresses it per consumer project when engaging.
		providerProjectCfg, err := consumer.ProjectRestConfig(cfg, csp.ProviderProject)
		if err != nil {
			setupLog.Error(err, "unable to build provider-project rest config")
			os.Exit(1)
		}

		providerMgr, err := ctrl.NewManager(providerProjectCfg, ctrl.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				// Avoid port conflict with the primary manager's metrics server.
				BindAddress: "0",
			},
		})
		if err != nil {
			setupLog.Error(err, "unable to create provider-project manager")
			os.Exit(1)
		}

		consumerOpts := consumer.Options{
			// mgr.GetClient() is pointed at the base Milo URL where cluster-scoped
			// Service objects live; providerMgr is project-scoped and cannot serve them.
			RootClient: mgr.GetClient(),
			// Canonical Service.spec.serviceName values the catalog owns.
			ServiceNames: csp.ServiceNames,
			// Engaged consumer clusters must use our scheme; without it their
			// cache falls back to the client-go global scheme and every
			// consumer-side LocationBinding / ServiceEntitlement watch fails
			// with "kind must be registered to the Scheme".
			ClusterOptions: []cluster.Option{
				func(o *cluster.Options) { o.Scheme = scheme },
			},
			// LocationBinding is the only type the catalog projects into
			// consumer projects; it is deleted (label-scoped) on deactivation.
			// Note: networking.datumapis.com uses version v1alpha (not v1alpha1).
			ManagedResources: []schema.GroupVersionKind{
				{Group: "networking.datumapis.com", Version: "v1alpha", Kind: "LocationBinding"},
			},
		}
		if csp.ResyncInterval != nil {
			consumerOpts.ResyncInterval = csp.ResyncInterval.Duration
		}

		consumerProvider, err := consumer.New(providerMgr, consumerOpts)
		if err != nil {
			setupLog.Error(err, "unable to create consumer provider")
			os.Exit(1)
		}

		consumerMcMgr, err := mcmanager.New(cfg, consumerProvider, mcmanager.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
		})
		if err != nil {
			setupLog.Error(err, "unable to create consumer multicluster manager")
			os.Exit(1)
		}

		if err = (&controller.LocationBindingReconciler{Scheme: scheme}).SetupWithManager(consumerMcMgr, mgr.GetClient()); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "LocationBinding")
			os.Exit(1)
		}

		// The provider watch (gated on provider-project readiness) and the consumer
		// multicluster manager each block, so each runs in its own goroutine —
		// mirroring the mcMgr wiring below. consumerProvider implements
		// multicluster.ProviderRunnable, so consumerMcMgr.Start calls
		// consumerProvider.Start automatically; no separate goroutine needed.
		// The consumer manager engages no local "" cluster: its membership is
		// exclusively the consumer projects the provider hands it.
		go func() {
			if err := consumer.WaitProviderProjectReady(ctx, cfg, csp.ProviderProject); err != nil {
				setupLog.Error(err, "provider project did not become ready")
				os.Exit(1)
			}
			if err := providerMgr.Start(ctx); err != nil {
				setupLog.Error(err, "provider-project manager failed")
				os.Exit(1)
			}
		}()
		go func() {
			if err := consumerMcMgr.Start(ctx); err != nil {
				setupLog.Error(err, "consumer multicluster manager failed")
				os.Exit(1)
			}
		}()

		setupLog.Info("consumer-scoped projection enabled",
			"providerProject", csp.ProviderProject, "serviceNames", csp.ServiceNames)
	} else {
		if err = (&controller.LocationBindingReconciler{Scheme: scheme}).SetupWithManager(mcMgr, mgr.GetClient()); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "LocationBinding")
			os.Exit(1)
		}
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
		if err = serviceswebhooks.SetupServiceEntitlementWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ServiceEntitlement")
			os.Exit(1)
		}
		if enableSingleClusterForE2ETests {
			// The ServiceConsumer webhook authorizes callers via a
			// SubjectAccessReview against "which project this request
			// targets", resolved from context milowebhook.ClusterAwareServer
			// injects from the admission request's UserInfo.Extra — stamped
			// in production by Milo's per-project aggregated-apiserver proxy.
			// e2esingle has no such proxy in front of it, so that context
			// never resolves and every write would be denied. Skip
			// registering it here rather than weaken its authorization
			// model; e2e exercises the reconciler/propagation path, not
			// this webhook's authorization logic.
			setupLog.Info("enable-single-cluster-for-e2e-tests is set: skipping ServiceConsumer webhook registration")
		} else if err = serviceswebhooks.SetupServiceConsumerWebhookWithManager(mgr, mcMgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ServiceConsumer")
			os.Exit(1)
		}
		if err = serviceswebhooks.SetupServiceAvailabilityWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ServiceAvailability")
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

	// Engage the local manager so the root cluster is reachable as cluster "".
	// The multicluster manager runs in the background — it and the primary
	// manager each block, and the provider's engagement loop runs as one of
	// its always-running runnables.
	//
	// Skipped under e2esingle: it engages the SAME underlying mgr under its
	// own fixed name already. The multicluster coordinator tracks engagement
	// by name, not cluster identity, so engaging that same cluster under ""
	// too would start a second, duplicate informer for every mcbuilder-based
	// watch (ServiceEntitlement, LocationBinding) — each create/update then
	// fires twice, once per name, and the ""-tagged copy is rejected by
	// reconcilers that require a non-empty cluster name.
	if !enableSingleClusterForE2ETests {
		go func() {
			if err := mcMgr.Engage(ctx, "", mcMgr.GetLocalManager()); err != nil {
				setupLog.Error(err, "unable to engage local cluster on multicluster manager")
				os.Exit(1)
			}
		}()
	}
	go func() {
		if err := mcMgr.Start(ctx); err != nil {
			setupLog.Error(err, "multicluster manager failed")
			os.Exit(1)
		}
	}()

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
}
