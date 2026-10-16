// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const deadlockConsumerProject = "proj-a"

// TestEngageDoesNotDeadlockControllerStartup runs the real controller-runtime
// and multicluster-runtime stack against an envtest API server and forces the
// interleaving behind milo-os/milo#840. A multicluster controller is starting:
// it holds its own lock while a root watch delivers its initial events, and
// that watch's handler looks the consumer project up through the provider.
// Meanwhile the provider engages the project, which needs the controller's
// lock. If the provider holds its lock across Engage, the handler, the
// controller and the provider wait on each other forever.
//
// Two gates make the ordering deterministic: the provider's cache sync does not
// return until the handler is running, and the handler does not look the
// project up until Engage has reached the controller.
func TestEngageDoesNotDeadlockControllerStartup(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS must be set in CI; this test needs an envtest API server")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set; run setup-envtest to provide an envtest API server")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "base", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, servicesv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	setup, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("setup client: %v", err)
	}
	createActiveConsumer(ctx, t, setup)
	if err := setup.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"},
	}); err != nil {
		t.Fatalf("create probe ConfigMap: %v", err)
	}

	handlerRunning := make(chan struct{})
	engaging := make(chan struct{})
	var handlerOnce, engagingOnce, engagedOnce sync.Once
	engaged := make(chan struct{})

	controllerDefaults := ctrlconfig.Controller{
		SkipNameValidation: ptr.To(true),
		CacheSyncTimeout:   5 * time.Minute,
	}
	providerMgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             controllerDefaults,
	})
	if err != nil {
		t.Fatalf("provider manager: %v", err)
	}

	p, err := New(providerMgr, Options{
		RootClient:   setup,
		ServiceNames: []string{computeCanonical},
		newCluster: func(_ *rest.Config, opts ...cluster.Option) (cluster.Cluster, error) {
			return cluster.New(cfg, append(opts, gatedProjectCluster(scheme, handlerRunning))...)
		},
	})
	if err != nil {
		t.Fatalf("consumer provider: %v", err)
	}

	consumerMcMgr, err := mcmanager.New(cfg, p, mcmanager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             controllerDefaults,
	})
	if err != nil {
		t.Fatalf("consumer multicluster manager: %v", err)
	}

	err = mcbuilder.ControllerManagedBy(consumerMcMgr).
		Named("consumer-engage-deadlock-fanout").
		WithOptions(mccontroller.Options{SkipNameValidation: ptr.To(true)}).
		For(&corev1.ConfigMap{},
			mcbuilder.WithEngageWithProviderClusters(true),
			mcbuilder.WithClusterFilter(func(name multicluster.ClusterName, _ cluster.Cluster) bool {
				if name == deadlockConsumerProject {
					engagingOnce.Do(func() { close(engaging) })
				}
				return true
			}),
		).
		WatchesRawSource(source.TypedKind(
			consumerMcMgr.GetLocalManager().GetCache(),
			&corev1.Namespace{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, _ *corev1.Namespace) []mcreconcile.Request {
				handlerOnce.Do(func() {
					close(handlerRunning)
					<-engaging
				})
				_, _ = consumerMcMgr.GetCluster(ctx, deadlockConsumerProject)
				return nil
			}),
		)).
		Complete(mcreconcile.Func(func(_ context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			if req.ClusterName == deadlockConsumerProject {
				engagedOnce.Do(func() { close(engaged) })
			}
			return ctrl.Result{}, nil
		}))
	if err != nil {
		t.Fatalf("fan-out controller: %v", err)
	}

	go func() { _ = providerMgr.Start(ctx) }()
	go func() { _ = consumerMcMgr.Start(ctx) }()

	select {
	case <-engaged:
	case <-time.After(60 * time.Second):
		t.Fatal("controller startup and consumer project engagement deadlocked: the project was never reconciled")
	}
}

func createActiveConsumer(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	svc := newService(computeObject, computeCanonical)
	svc.Spec.Owner.ProducerProjectRef.Name = "provider-project"
	if err := c.Create(ctx, svc); err != nil {
		t.Fatalf("create Service: %v", err)
	}
	sc := newConsumer("c-"+deadlockConsumerProject, computeObject, deadlockConsumerProject, servicesv1alpha1.ConsumerPhaseActive)
	status := sc.Status
	if err := c.Create(ctx, sc); err != nil {
		t.Fatalf("create ServiceConsumer: %v", err)
	}
	sc.Status = status
	if err := c.Status().Update(ctx, sc); err != nil {
		t.Fatalf("mark ServiceConsumer Active: %v", err)
	}
}

// gatedProjectCluster holds the consumer cluster's first cache sync until the
// fan-out handler is running, so Engage reaches a controller that is still
// starting.
func gatedProjectCluster(scheme *runtime.Scheme, handlerRunning <-chan struct{}) cluster.Option {
	var once sync.Once
	return func(o *cluster.Options) {
		o.Scheme = scheme
		o.NewCache = func(config *rest.Config, opts cache.Options) (cache.Cache, error) {
			c, err := cache.New(config, opts)
			if err != nil {
				return nil, err
			}
			return &handlerGatedCache{Cache: c, once: &once, wait: handlerRunning}, nil
		}
	}
}

type handlerGatedCache struct {
	cache.Cache
	once *sync.Once
	wait <-chan struct{}
}

func (c *handlerGatedCache) WaitForCacheSync(ctx context.Context) bool {
	synced := c.Cache.WaitForCacheSync(ctx)
	c.once.Do(func() {
		select {
		case <-c.wait:
		case <-ctx.Done():
		}
	})
	return synced
}
