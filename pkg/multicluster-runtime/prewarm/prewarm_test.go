// SPDX-License-Identifier: AGPL-3.0-only

package prewarm

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// fakeProjectCache records the informers asked of it. WaitForCacheSync
// reports synced once synced is closed, or at once when synced is nil.
type fakeProjectCache struct {
	cache.Cache
	synced chan struct{}

	mu      sync.Mutex
	started []string
}

func (c *fakeProjectCache) GetInformer(_ context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	var o cache.InformerGetOptions
	for _, opt := range opts {
		opt(&o)
	}
	blocking := o.BlockUntilSynced == nil || *o.BlockUntilSynced
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, fmt.Sprintf("%T blocking=%t", obj, blocking))
	return nil, nil
}

func (c *fakeProjectCache) WaitForCacheSync(ctx context.Context) bool {
	if c.synced == nil {
		return true
	}
	select {
	case <-c.synced:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *fakeProjectCache) startedInformers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := append([]string(nil), c.started...)
	sort.Strings(got)
	return got
}

// projectCache builds a project cache the way a provider does, through the
// cluster option.
func projectCache(t *testing.T, kinds *WatchedKinds, fake cache.Cache) cache.Cache {
	t.Helper()
	opts := &cluster.Options{NewCache: func(*rest.Config, cache.Options) (cache.Cache, error) { return fake, nil }}
	kinds.ClusterOption()(opts)
	c, err := opts.NewCache(&rest.Config{}, cache.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := servicesv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func location() client.Object {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "locations.miloapis.com", Version: "v1alpha1", Kind: "Location"})
	return u
}

// A project's informers must be started before it is engaged, for every kind
// the controllers watched on the projects engaged before it, so engaging it
// leaves the controllers nothing to list while they hold their locks.
func TestPrewarmingCacheStartsWatchedKindsBeforeEngagement(t *testing.T) {
	ctx := context.Background()
	kinds := NewWatchedKinds()

	// Controllers watch these on the first project as it engages.
	first := projectCache(t, kinds, &fakeProjectCache{})
	for _, obj := range []client.Object{&servicesv1alpha1.ServiceEntitlement{}, location(), &servicesv1alpha1.ServiceEntitlement{}} {
		if _, err := first.GetInformer(ctx, obj); err != nil {
			t.Fatalf("GetInformer: %v", err)
		}
	}

	fake := &fakeProjectCache{}
	next := projectCache(t, kinds, fake)
	if !next.WaitForCacheSync(ctx) {
		t.Fatal("WaitForCacheSync = false, want true")
	}
	want := []string{
		"*unstructured.Unstructured blocking=false",
		"*v1alpha1.ServiceEntitlement blocking=false",
	}
	if got := fake.startedInformers(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("informers started before engagement = %v, want %v", got, want)
	}

	// Another provider's controllers watch other kinds; its projects must not
	// start these.
	other := &fakeProjectCache{}
	if !projectCache(t, NewWatchedKinds(), other).WaitForCacheSync(ctx) {
		t.Fatal("WaitForCacheSync = false, want true")
	}
	if got := other.startedInformers(); len(got) != 0 {
		t.Errorf("another provider's project started %v, want none", got)
	}
}

// An informer that never syncs must not hold a project back forever on the
// prewarm: engagement goes ahead and waits on it as it did before.
func TestPrewarmingCacheGivesUpAfterTimeout(t *testing.T) {
	prev := InformerTimeout
	InformerTimeout = 20 * time.Millisecond
	t.Cleanup(func() { InformerTimeout = prev })

	kinds := NewWatchedKinds()
	c := projectCache(t, kinds, &fakeProjectCache{synced: make(chan struct{})})
	if !c.WaitForCacheSync(context.Background()) {
		t.Error("WaitForCacheSync = false after the prewarm timeout, want true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.WaitForCacheSync(ctx) {
		t.Error("WaitForCacheSync = true with a cancelled context, want false")
	}
}

// listingCache is a project cache whose informers each take listDelay to
// list, the way a project's informers do against a busy Milo. A blocking
// GetInformer waits for that list; WaitForCacheSync waits for every informer
// started so far.
type listingCache struct {
	cache.Cache
	listDelay time.Duration

	mu    sync.Mutex
	kinds map[string]chan struct{}
}

func (c *listingCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	var o cache.InformerGetOptions
	for _, opt := range opts {
		opt(&o)
	}
	c.mu.Lock()
	if c.kinds == nil {
		c.kinds = map[string]chan struct{}{}
	}
	listed, ok := c.kinds[fmt.Sprintf("%T", obj)]
	if !ok {
		listed = make(chan struct{})
		c.kinds[fmt.Sprintf("%T", obj)] = listed
		time.AfterFunc(c.listDelay, func() { close(listed) })
	}
	c.mu.Unlock()

	if o.BlockUntilSynced == nil || *o.BlockUntilSynced {
		select {
		case <-listed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &controllertest.FakeInformer{Synced: true}, nil
}

func (c *listingCache) WaitForCacheSync(ctx context.Context) bool {
	c.mu.Lock()
	var pending []chan struct{}
	for _, listed := range c.kinds {
		pending = append(pending, listed)
	}
	c.mu.Unlock()
	for _, listed := range pending {
		select {
		case <-listed:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// projectCluster is a project control plane with only a cache.
type projectCluster struct {
	cluster.Cluster
	cache cache.Cache
}

func (c *projectCluster) GetCache() cache.Cache { return c.cache }

// engageProjects engages projects with every controller at once, the way
// the provider's engagement workers do: wait for the project's cache, then
// Engage. It returns how long the whole pass took.
func engageProjects(t *testing.T, ctx context.Context, controllers []mccontroller.Controller, newCache func() cache.Cache, projects []string) time.Duration {
	t.Helper()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, len(projects))
	for _, name := range projects {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			cl := &projectCluster{cache: newCache()}
			if !cl.cache.WaitForCacheSync(ctx) {
				errs <- fmt.Errorf("%s: cache did not sync", name)
				return
			}
			for _, c := range controllers {
				if err := c.Engage(ctx, multicluster.ClusterName(name), cl); err != nil {
					errs <- fmt.Errorf("%s: %w", name, err)
					return
				}
			}
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return time.Since(start)
}

// Engaging a project starts each controller's watches on it while holding
// that controller's lock, so without prewarming one controller lists for one
// project at a time however many workers engage projects. With prewarming
// the lists happen before Engage, side by side, and a whole batch of
// projects engages in about the time one takes to list.
func TestPrewarmedProjectsEngageInParallel(t *testing.T) {
	const (
		listDelay = 50 * time.Millisecond
		batch     = 16
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newControllers := func(prefix string) []mccontroller.Controller {
		return runningControllers(t, ctx, prefix, []client.Object{
			&servicesv1alpha1.ServiceEntitlement{},
			&servicesv1alpha1.ServiceActivationRequest{},
			&servicesv1alpha1.ServiceConsumer{},
		})
	}

	names := func(prefix string, n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("%s-%d", prefix, i))
		}
		return out
	}

	serial := engageProjects(t, ctx, newControllers("plain"),
		func() cache.Cache { return &listingCache{listDelay: listDelay} }, names("plain", batch))

	kinds := NewWatchedKinds()
	newPrewarmed := func() cache.Cache { return projectCache(t, kinds, &listingCache{listDelay: listDelay}) }
	prewarmedControllers := newControllers("prewarmed")
	// The first project teaches the provider which kinds its controllers watch.
	engageProjects(t, ctx, prewarmedControllers, newPrewarmed, names("first", 1))
	parallel := engageProjects(t, ctx, prewarmedControllers, newPrewarmed, names("prewarmed", batch))

	t.Logf("%d projects: %v without prewarming, %v with", batch, serial, parallel)
	if serial < batch*listDelay {
		t.Fatalf("without prewarming %d projects engaged in %v, want at least %v; the test no longer reproduces serialized engagement",
			batch, serial, batch*listDelay)
	}
	if parallel > batch*listDelay/4 {
		t.Errorf("with prewarming %d projects engaged in %v, want under %v", batch, parallel, batch*listDelay/4)
	}
}

// runningControllers starts one multicluster controller per kind, each
// watching its kind on every project it engages.
func runningControllers(t *testing.T, ctx context.Context, prefix string, objs []client.Object) []mccontroller.Controller {
	t.Helper()
	var controllers []mccontroller.Controller
	for i, obj := range objs {
		c, err := mccontroller.NewUnmanaged(fmt.Sprintf("%s-%d", prefix, i), nil, mccontroller.Options{
			Reconciler: mcreconcile.Func(func(context.Context, mcreconcile.Request) (reconcile.Result, error) {
				return reconcile.Result{}, nil
			}),
			SkipNameValidation: ptr.To(true),
		})
		if err != nil {
			t.Fatalf("controller: %v", err)
		}
		if err := c.MultiClusterWatch(mcsource.Kind(obj, mchandler.EnqueueRequestForObject)); err != nil {
			t.Fatalf("watch: %v", err)
		}
		// Engagement after a restart happens once controllers run;
		// before that, Watch only queues the source.
		running := make(chan struct{})
		if err := c.Watch(source.TypedFunc[mcreconcile.Request](func(context.Context, workqueue.TypedRateLimitingInterface[mcreconcile.Request]) error {
			close(running)
			return nil
		})); err != nil {
			t.Fatalf("watch: %v", err)
		}
		go func() { _ = c.Start(ctx) }()
		<-running
		controllers = append(controllers, c)
	}
	return controllers
}

// A provider that engages one project at a time, like the consumer provider,
// still waits on each controller's list in turn without prewarming. With
// prewarming the project lists every watched kind at once and engages in
// about the time one list takes. Compute's leader engaged a new project 9s
// after its non-leader for this reason.
func TestPrewarmedProjectListsWatchedKindsAtOnce(t *testing.T) {
	const listDelay = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	objs := []client.Object{
		&servicesv1alpha1.ServiceEntitlement{},
		&servicesv1alpha1.ServiceActivationRequest{},
		&servicesv1alpha1.ServiceConsumer{},
		&servicesv1alpha1.Service{},
		&servicesv1alpha1.ServiceAgent{},
		&servicesv1alpha1.ServiceConfiguration{},
	}

	serial := engageProjects(t, ctx, runningControllers(t, ctx, "single-plain", objs),
		func() cache.Cache { return &listingCache{listDelay: listDelay} }, []string{"plain"})

	kinds := NewWatchedKinds()
	newPrewarmed := func() cache.Cache { return projectCache(t, kinds, &listingCache{listDelay: listDelay}) }
	controllers := runningControllers(t, ctx, "single-prewarmed", objs)
	engageProjects(t, ctx, controllers, newPrewarmed, []string{"first"})
	prewarmed := engageProjects(t, ctx, controllers, newPrewarmed, []string{"next"})

	t.Logf("one project with %d watched kinds: %v without prewarming, %v with", len(objs), serial, prewarmed)
	if serial < time.Duration(len(objs))*listDelay {
		t.Fatalf("without prewarming the project engaged in %v, want at least %v; the test no longer reproduces serialized listing",
			serial, time.Duration(len(objs))*listDelay)
	}
	if prewarmed > 3*listDelay {
		t.Errorf("with prewarming the project engaged in %v, want under %v", prewarmed, 3*listDelay)
	}
}
