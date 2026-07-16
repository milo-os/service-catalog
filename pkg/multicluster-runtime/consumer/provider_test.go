// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// --- test scheme & fixtures ---------------------------------------------------

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := servicesv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add services scheme: %v", err)
	}
	return s
}

// newProviderClient builds a fake provider-project client with the SAME field
// index the real Provider registers (spec.serviceRef.name). computeActiveSet
// lists ServiceConsumers via client.MatchingFields on that index; without it the
// fake client errors on the query, so registering it here is mandatory.
func newProviderClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithIndex(&servicesv1alpha1.ServiceConsumer{}, serviceRefNameField, func(obj client.Object) []string {
			sc, ok := obj.(*servicesv1alpha1.ServiceConsumer)
			if !ok {
				return nil
			}
			return []string{sc.Spec.ServiceRef.Name}
		}).
		WithObjects(objs...).
		Build()
}

func newService(objName, canonical string) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: objName},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: canonical,
			DisplayName: objName,
			Phase:       servicesv1alpha1.PhasePublished,
		},
	}
}

func newConsumer(name, serviceObjName, project string, phase servicesv1alpha1.ConsumerPhase) *servicesv1alpha1.ServiceConsumer {
	return &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: serviceObjName},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: project},
		},
		Status: servicesv1alpha1.ServiceConsumerStatus{Phase: phase},
	}
}

// deletingConsumer is an Active consumer mid-deletion (DeletionTimestamp set).
// The fake client requires a finalizer for an object that carries a deletion
// timestamp, so one is attached.
func deletingConsumer(name, serviceObjName, project string) *servicesv1alpha1.ServiceConsumer {
	now := metav1.Now()
	sc := newConsumer(name, serviceObjName, project, servicesv1alpha1.ConsumerPhaseActive)
	sc.Finalizers = []string{"test.miloapis.com/keep"}
	sc.DeletionTimestamp = &now
	return sc
}

// --- fake cluster / cache / mcmanager ----------------------------------------

// fakeCache satisfies cache.Cache but only implements the two methods engage
// touches: WaitForCacheSync (must report synced) and IndexField (no-op). Every
// other method is the embedded nil interface and must never be called.
type fakeCache struct {
	cache.Cache
	indexed  []string // fields IndexField was asked to apply
	failSync bool     // when true, WaitForCacheSync reports NOT synced (timeout)
}

func (c *fakeCache) WaitForCacheSync(context.Context) bool { return !c.failSync }
func (c *fakeCache) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	c.indexed = append(c.indexed, field)
	return nil
}

// fakeCluster satisfies cluster.Cluster. engage uses GetCache (sync + index) and
// Start; the rest return zero values.
type fakeCluster struct {
	client client.Client
	cache  *fakeCache
}

func (c *fakeCluster) GetClient() client.Client                        { return c.client }
func (c *fakeCluster) GetCache() cache.Cache                           { return c.cache }
func (c *fakeCluster) GetScheme() *runtime.Scheme                      { return nil }
func (c *fakeCluster) GetHTTPClient() *http.Client                     { return nil }
func (c *fakeCluster) GetConfig() *rest.Config                         { return nil }
func (c *fakeCluster) GetFieldIndexer() client.FieldIndexer            { return c.cache }
func (c *fakeCluster) GetEventRecorderFor(string) record.EventRecorder { return nil }
func (c *fakeCluster) GetEventRecorder(string) events.EventRecorder    { return nil }
func (c *fakeCluster) GetRESTMapper() meta.RESTMapper                  { return nil }
func (c *fakeCluster) GetAPIReader() client.Reader                     { return nil }
func (c *fakeCluster) Start(context.Context) error                     { return nil }

// fakeMCMgr satisfies multicluster.Aware. Only Engage is exercised; it records
// the context each engagement was started with so tests can assert the
// per-cluster context is cancelled on disengage. engageErr, when set, forces
// Engage to fail (exercising engage's cleanup path).
type fakeMCMgr struct {
	engaged   map[string]context.Context
	engageErr error
}

func newFakeMCMgr() *fakeMCMgr {
	return &fakeMCMgr{engaged: map[string]context.Context{}}
}

func (m *fakeMCMgr) Engage(ctx context.Context, name multicluster.ClusterName, _ cluster.Cluster) error {
	if m.engageErr != nil {
		return m.engageErr
	}
	m.engaged[string(name)] = ctx
	return nil
}

// --- provider constructor for white-box tests --------------------------------

type providerOpt func(*Provider)

func withMCMgr(m multicluster.Aware) providerOpt { return func(p *Provider) { p.aware = m } }

func withRootClient(c client.Client) providerOpt { return func(p *Provider) { p.rootClient = c } }

func withNewCluster(fn func(*rest.Config, ...cluster.Option) (cluster.Cluster, error)) providerOpt {
	return func(p *Provider) { p.newCluster = fn }
}

func withNewClient(fn func(*rest.Config, client.Options) (client.Client, error)) providerOpt {
	return func(p *Provider) { p.newClient = fn }
}

func withManagedResources(gvks ...schema.GroupVersionKind) providerOpt {
	return func(p *Provider) { p.opts.ManagedResources = gvks }
}

func withTeardowns(tds ...Teardown) providerOpt {
	return func(p *Provider) { p.opts.Teardowns = tds }
}

func newTestProvider(providerClient client.Client, serviceNames []string, opts ...providerOpt) *Provider {
	sn := make(map[string]struct{}, len(serviceNames))
	for _, n := range serviceNames {
		sn[n] = struct{}{}
	}
	p := &Provider{
		opts:               Options{ServiceNames: serviceNames},
		log:                logr.Discard(),
		rootClient:         providerClient,
		providerClient:     providerClient,
		providerRestConfig: &rest.Config{Host: "https://localhost"},
		resyncInterval:     DefaultResyncInterval,
		newCluster: func(*rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			return &fakeCluster{cache: &fakeCache{}}, nil
		},
		serviceNames: sn,
		clusters:     map[string]cluster.Cluster{},
		cancelFns:    map[string]context.CancelFunc{},
		tearingDown:  map[string]struct{}{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

const (
	computeCanonical = "compute.miloapis.com"
	computeObject    = "compute-miloapis-com"
	storageCanonical = "storage.miloapis.com"
	storageObject    = "storage-miloapis-com"
)

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- computeActiveSet ---------------------------------------------------------

func TestComputeActiveSet_CanonicalJoinAndFilters(t *testing.T) {
	// Owned: compute (canonical in ServiceNames). Not owned: storage.
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newService(storageObject, storageCanonical),

		// Active consumers for the owned service across two projects -> both active.
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
		newConsumer("c-proj-b", computeObject, "proj-b", servicesv1alpha1.ConsumerPhaseActive),

		// Non-active phases for the owned service -> excluded.
		newConsumer("c-pending", computeObject, "proj-pending", servicesv1alpha1.ConsumerPhasePendingApproval),
		newConsumer("c-denied", computeObject, "proj-denied", servicesv1alpha1.ConsumerPhaseDenied),

		// Active consumer mid-deletion -> excluded.
		deletingConsumer("c-deleting", computeObject, "proj-deleting"),

		// Active consumer for a service NOT in ServiceNames -> excluded (proves the
		// canonical join: only consumers whose Service.spec.serviceName is owned count).
		newConsumer("c-storage", storageObject, "proj-storage", servicesv1alpha1.ConsumerPhaseActive),
	}

	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical})

	active, revoked, err := p.computeActiveSet(context.Background())
	if err != nil {
		t.Fatalf("computeActiveSet: %v", err)
	}

	want := map[string]struct{}{"proj-a": {}, "proj-b": {}}
	if len(active) != len(want) {
		t.Fatalf("active set = %v, want %v", keys(active), keys(want))
	}
	for p := range want {
		if _, ok := active[p]; !ok {
			t.Errorf("expected project %q active, got set %v", p, keys(active))
		}
	}
	for _, bad := range []string{"proj-pending", "proj-denied", "proj-deleting", "proj-storage"} {
		if _, ok := active[bad]; ok {
			t.Errorf("project %q must not be active (set=%v)", bad, keys(active))
		}
	}

	// Denied and being-deleted consumers land in the REVOKED set (teardown
	// candidates that may hold projected resources but are no longer entitled).
	// Pending-only and not-owned projects appear in NEITHER set.
	wantRevoked := map[string]struct{}{"proj-denied": {}, "proj-deleting": {}}
	if len(revoked) != len(wantRevoked) {
		t.Fatalf("revoked set = %v, want %v", keys(revoked), keys(wantRevoked))
	}
	for p := range wantRevoked {
		if _, ok := revoked[p]; !ok {
			t.Errorf("expected project %q revoked, got set %v", p, keys(revoked))
		}
	}
	for _, notRevoked := range []string{"proj-a", "proj-b", "proj-pending", "proj-storage"} {
		if _, ok := revoked[notRevoked]; ok {
			t.Errorf("project %q must not be revoked (set=%v)", notRevoked, keys(revoked))
		}
	}
}

func TestComputeActiveSet_NoOwnedServices(t *testing.T) {
	// A consumer exists, but its service's canonical name is not owned.
	objs := []client.Object{
		newService(storageObject, storageCanonical),
		newConsumer("c-storage", storageObject, "proj-storage", servicesv1alpha1.ConsumerPhaseActive),
	}
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical})

	active, revoked, err := p.computeActiveSet(context.Background())
	if err != nil {
		t.Fatalf("computeActiveSet: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("expected empty active set, got %v", keys(active))
	}
	if len(revoked) != 0 {
		t.Errorf("expected empty revoked set, got %v", keys(revoked))
	}
}

func TestComputeActiveSet_ReadsServicesFromBaseClient(t *testing.T) {
	// Service is cluster-scoped and lives in the base Milo cluster, not the
	// provider project. Put each type on the client that would serve it in
	// production; if computeActiveSet reads Services from the wrong client it
	// finds nothing and the active set is empty.
	serviceClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(newService(computeObject, computeCanonical)).
		Build()
	providerClient := newProviderClient(t,
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
	)

	p := newTestProvider(providerClient, []string{computeCanonical},
		withRootClient(serviceClient),
	)

	active, _, err := p.computeActiveSet(context.Background())
	if err != nil {
		t.Fatalf("computeActiveSet: %v", err)
	}
	if _, ok := active["proj-a"]; !ok {
		t.Errorf("expected proj-a in active set (got %v); Service is cluster-scoped and must be read from the base Milo client, not the provider-project client", keys(active))
	}
}

// --- Get sentinel -------------------------------------------------------------

func TestGet_WrapsErrClusterNotFoundWhenUnengaged(t *testing.T) {
	p := newTestProvider(newProviderClient(t), []string{computeCanonical})

	cl, err := p.Get(context.Background(), "proj-x")
	if cl != nil {
		t.Errorf("expected nil cluster for unengaged project, got %v", cl)
	}
	if err == nil {
		t.Fatalf("expected error for unengaged project")
	}
	if !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("error %v must wrap multicluster.ErrClusterNotFound (errors.Is)", err)
	}
}

func TestGet_ReturnsEngagedCluster(t *testing.T) {
	p := newTestProvider(newProviderClient(t), []string{computeCanonical})
	want := &fakeCluster{cache: &fakeCache{}}
	p.clusters["proj-x"] = want

	got, err := p.Get(context.Background(), "proj-x")
	if err != nil {
		t.Fatalf("Get engaged: %v", err)
	}
	if got != want {
		t.Errorf("Get returned %v, want the engaged cluster %v", got, want)
	}
}

// --- Reconcile: engage / disengage deltas ------------------------------------

func TestReconcile_EngagesNewlyActiveProject(t *testing.T) {
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
	}
	mcMgr := newFakeMCMgr()
	calls := 0
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical},
		withMCMgr(mcMgr),
		withNewCluster(func(*rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			calls++
			return &fakeCluster{cache: &fakeCache{}}, nil
		}),
	)

	res, err := p.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != p.resyncInterval {
		t.Errorf("RequeueAfter = %v, want resyncInterval %v", res.RequeueAfter, p.resyncInterval)
	}
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Errorf("expected proj-a engaged, clusters=%v", p.clusters)
	}
	if _, ok := mcMgr.engaged["proj-a"]; !ok {
		t.Errorf("expected mcMgr.Engage called for proj-a")
	}
	if calls != 1 {
		t.Errorf("newCluster called %d times, want 1", calls)
	}
}

func TestReconcile_Idempotent_NoReEngage(t *testing.T) {
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
	}
	calls := 0
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withNewCluster(func(*rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			calls++
			return &fakeCluster{cache: &fakeCache{}}, nil
		}),
	)

	for i := range 3 {
		if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("newCluster called %d times across 3 reconciles, want 1 (already-engaged is a no-op)", calls)
	}
	if len(p.clusters) != 1 {
		t.Errorf("expected exactly 1 engaged cluster, got %d", len(p.clusters))
	}
}

func TestReconcile_DisengagesAndCancelsWhenNoLongerActive(t *testing.T) {
	consumer := newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive)
	providerClient := newProviderClient(t,
		newService(computeObject, computeCanonical),
		consumer,
	)
	mcMgr := newFakeMCMgr()
	p := newTestProvider(providerClient, []string{computeCanonical}, withMCMgr(mcMgr))

	// First reconcile engages proj-a.
	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile (engage): %v", err)
	}
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Fatalf("precondition: proj-a should be engaged")
	}
	engageCtx := mcMgr.engaged["proj-a"]
	if engageCtx == nil {
		t.Fatalf("precondition: expected engage context recorded")
	}
	if engageCtx.Err() != nil {
		t.Fatalf("precondition: engage context must be live before disengage")
	}

	// Consumer drops out of the active set (deleted -> no active reference).
	if err := providerClient.Delete(context.Background(),
		&servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: consumer.Name}}); err != nil {
		t.Fatalf("delete consumer: %v", err)
	}

	// Second reconcile disengages proj-a.
	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile (disengage): %v", err)
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("expected proj-a disengaged, clusters=%v", p.clusters)
	}
	if _, ok := p.cancelFns["proj-a"]; ok {
		t.Errorf("expected proj-a cancelFn removed")
	}
	// Cancel-before-anything: the per-cluster context must be cancelled.
	if engageCtx.Err() == nil {
		t.Errorf("expected per-cluster context cancelled on disengage")
	}
	// Get now reports it unengaged with the wrapped sentinel.
	if _, err := p.Get(context.Background(), "proj-a"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("after disengage, Get should wrap ErrClusterNotFound, got %v", err)
	}
}

func TestReconcile_EngageFailureDoesNotRegisterCluster(t *testing.T) {
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
	}
	mcMgr := newFakeMCMgr()
	mcMgr.engageErr = errors.New("engage boom")
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical}, withMCMgr(mcMgr))

	_, err := p.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Fatalf("expected reconcile to surface the engage error")
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("failed engage must not leave proj-a in the engaged set")
	}
	if _, ok := p.cancelFns["proj-a"]; ok {
		t.Errorf("failed engage must not leave a cancelFn for proj-a")
	}
}

// --- engage: bounded WaitForCacheSync timeout --------------------------------

// When a consumer cache never syncs (WaitForCacheSync returns false under the
// bounded cacheSyncTimeout), engage must fail cleanly: no cluster registered, no
// Engage call, and the per-cluster context cancelled — so one unreachable
// consumer can't head-of-line-block others.
func TestReconcile_CacheSyncFailureDoesNotEngage(t *testing.T) {
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
	}
	mcMgr := newFakeMCMgr()
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical},
		withMCMgr(mcMgr),
		withNewCluster(func(*rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			return &fakeCluster{cache: &fakeCache{failSync: true}}, nil
		}),
	)

	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatalf("expected engage to fail when the consumer cache never syncs")
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("cluster must NOT be registered when cache sync fails")
	}
	if _, ok := p.cancelFns["proj-a"]; ok {
		t.Errorf("no cancelFn should be retained when cache sync fails")
	}
	if _, ok := mcMgr.engaged["proj-a"]; ok {
		t.Errorf("mcMgr.Engage must not be called when cache sync fails")
	}
	if _, err := p.Get(context.Background(), "proj-a"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("after failed engage Get should wrap ErrClusterNotFound, got %v", err)
	}
}

// --- Reconcile: aware==nil requeue guard -------------------------------------

func TestReconcile_RequeuesWhenMcMgrUnbound(t *testing.T) {
	// No withMCMgr -> aware is nil (Start not yet called). Provider client is given
	// but must not be consulted before the guard fires.
	p := newTestProvider(newProviderClient(t), []string{computeCanonical})
	if p.aware != nil {
		t.Fatalf("precondition: aware must be nil")
	}

	res, err := p.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("reconcile must not error on unbound mcMgr: %v", err)
	}
	if res.RequeueAfter != mcMgrUnboundRequeue {
		t.Errorf("RequeueAfter = %v, want mcMgrUnboundRequeue %v", res.RequeueAfter, mcMgrUnboundRequeue)
	}
	if len(p.clusters) != 0 {
		t.Errorf("no engagement should happen while mcMgr is unbound, clusters=%v", p.clusters)
	}
}

// --- Start binds aware --------------------------------------------------------

func TestStart_BindsAwareThenReturnsOnContextCancel(t *testing.T) {
	p := newTestProvider(newProviderClient(t), []string{computeCanonical})
	mcMgr := newFakeMCMgr()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx, mcMgr) }()

	// Cancel and confirm Start returns; aware must be bound by then.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Start returned %v, want context.Canceled", err)
	}
	p.lock.Lock()
	bound := p.aware
	p.lock.Unlock()
	if bound == nil {
		t.Errorf("Start must bind aware")
	}
}

// --- IndexField fan-out -------------------------------------------------------

func TestIndexField_RecordsAndAppliesToEngagedClusters(t *testing.T) {
	p := newTestProvider(newProviderClient(t), []string{computeCanonical})
	fc := &fakeCluster{cache: &fakeCache{}}
	p.clusters["proj-a"] = fc

	err := p.IndexField(context.Background(), &servicesv1alpha1.ServiceConsumer{}, "spec.demo", func(client.Object) []string { return nil })
	if err != nil {
		t.Fatalf("IndexField: %v", err)
	}

	// Recorded for future engagements.
	found := false
	for _, idx := range p.indexers {
		if idx.field == "spec.demo" {
			found = true
		}
	}
	if !found {
		t.Errorf("IndexField must record the indexer for future clusters")
	}
	// Applied to the already-engaged cluster's cache.
	if len(fc.cache.indexed) != 1 || fc.cache.indexed[0] != "spec.demo" {
		t.Errorf("expected index applied to engaged cluster cache, got %v", fc.cache.indexed)
	}
}

// --- ProjectRestConfig host rewrite ------------------------------------------

func TestProjectRestConfig_HappyPathDoesNotMutateBase(t *testing.T) {
	base := &rest.Config{Host: "https://milo.example.com", BearerToken: "secret"}

	got, err := ProjectRestConfig(base, "p")
	if err != nil {
		t.Fatalf("ProjectRestConfig: %v", err)
	}

	want := "https://milo.example.com/apis/resourcemanager.miloapis.com/v1alpha1/projects/p/control-plane"
	if got.Host != want {
		t.Errorf("rewritten host = %q, want %q", got.Host, want)
	}
	// Carries the rest of the config forward (it's a copy of base).
	if got.BearerToken != "secret" {
		t.Errorf("expected base config fields copied, BearerToken = %q", got.BearerToken)
	}
	// Must NOT mutate the caller's base config (rest.CopyConfig).
	if base.Host != "https://milo.example.com" {
		t.Errorf("base.Host was mutated to %q; ProjectRestConfig must copy", base.Host)
	}
}

func TestProjectRestConfig_Errors(t *testing.T) {
	if _, err := ProjectRestConfig(nil, "p"); err == nil {
		t.Errorf("expected error for nil base rest.Config")
	}
	if _, err := ProjectRestConfig(&rest.Config{Host: "https://milo.example.com"}, ""); err == nil {
		t.Errorf("expected error for empty project")
	}
}

// --- Phase 2: deactivation cleanup (ManagedResources + Teardown) -------------

var locationBindingGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha",
	Kind:    "LocationBinding",
}

// recordedDelete captures one DeleteAllOf call: the addressed GVK, the label
// selector string, and whether the per-cluster context was already cancelled at
// call time (so tests can prove cancel-precedes-delete ordering).
type recordedDelete struct {
	gvk      schema.GroupVersionKind
	selector string
	ctxDone  bool
}

// recordingClient is the non-cached "direct" client the teardown path uses. It
// implements client.Client by embedding the interface (nil) and overrides only
// DeleteAllOf — every other method panics if unexpectedly called, which is a
// useful guard that teardown touches nothing else. LocationBinding is an
// external unstructured GVK absent from our scheme, so recording the requests
// (rather than a real fake client) is the cleanest way to assert label-scoping.
type recordingClient struct {
	client.Client
	mu       sync.Mutex
	deletes  []recordedDelete
	err      error                  // when set, DeleteAllOf returns it
	watchCtx func() context.Context // the per-cluster engage ctx, for ordering checks
}

func (c *recordingClient) DeleteAllOf(_ context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	o := &client.DeleteAllOfOptions{}
	for _, opt := range opts {
		opt.ApplyToDeleteAllOf(o)
	}
	sel := ""
	if o.LabelSelector != nil {
		sel = o.LabelSelector.String()
	}
	done := false
	if c.watchCtx != nil {
		if wc := c.watchCtx(); wc != nil {
			done = wc.Err() != nil
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, recordedDelete{
		gvk:      obj.GetObjectKind().GroupVersionKind(),
		selector: sel,
		ctxDone:  done,
	})
	return c.err
}

// recordingTeardown is a Teardown test double recording its invocations, the
// client it was handed, and whether the per-cluster ctx was cancelled when it ran.
type recordingTeardown struct {
	err       error
	calls     int
	gotClient client.Client
	gotNames  []string
	ctxDone   []bool
	watchCtx  func() context.Context
}

func (t *recordingTeardown) TeardownConsumer(_ context.Context, _ string, c client.Client, names []string) error {
	t.calls++
	t.gotClient = c
	t.gotNames = names
	if t.watchCtx != nil {
		wc := t.watchCtx()
		t.ctxDone = append(t.ctxDone, wc != nil && wc.Err() != nil)
	}
	return t.err
}

// engageStub puts the provider into the "engaged" state for a project without
// going through the full Reconcile/engage path, returning the per-cluster
// context that disengage will cancel.
func engageStub(p *Provider, project string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	p.lock.Lock()
	p.clusters[project] = &fakeCluster{cache: &fakeCache{}}
	p.cancelFns[project] = cancel
	p.lock.Unlock()
	return ctx
}

func TestDisengage_DeletesManagedResourcesLabelScoped(t *testing.T) {
	rec := &recordingClient{}
	td := &recordingTeardown{}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	clusterCtx := engageStub(p, "proj-a")
	rec.watchCtx = func() context.Context { return clusterCtx }
	td.watchCtx = func() context.Context { return clusterCtx }

	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("disengage: %v", err)
	}

	// Exactly one DeleteAllOf: 1 GVK x 1 serviceName, scoped to the canonical label.
	if len(rec.deletes) != 1 {
		t.Fatalf("expected 1 DeleteAllOf, got %d: %+v", len(rec.deletes), rec.deletes)
	}
	d := rec.deletes[0]
	if d.gvk != locationBindingGVK {
		t.Errorf("delete gvk = %v, want %v", d.gvk, locationBindingGVK)
	}
	wantSel := labelServiceName + "=" + computeCanonical
	if d.selector != wantSel {
		t.Errorf("delete selector = %q, want %q", d.selector, wantSel)
	}
	// Label-scoping guards: never the coarse managed-by label, never a sibling
	// service's label. A sibling's labelled object is out of selector scope.
	if strings.Contains(d.selector, "managed-by") {
		t.Errorf("teardown must never delete by the managed-by label, got %q", d.selector)
	}
	if strings.Contains(d.selector, storageCanonical) {
		t.Errorf("teardown must not target a sibling service's label, got %q", d.selector)
	}
	// Cancel precedes delete.
	if !d.ctxDone {
		t.Errorf("per-cluster ctx must be cancelled BEFORE ManagedResources delete (cancel-precedes-teardown)")
	}

	// Teardown ran once, after cancel, with the SAME direct (non-cached) client.
	if td.calls != 1 {
		t.Errorf("teardown calls = %d, want 1", td.calls)
	}
	if td.gotClient != rec {
		t.Errorf("teardown must receive the direct (non-cached) client")
	}
	if len(td.ctxDone) != 1 || !td.ctxDone[0] {
		t.Errorf("teardown must run after the per-cluster ctx is cancelled, ctxDone=%v", td.ctxDone)
	}
	if len(td.gotNames) != 1 || td.gotNames[0] != computeCanonical {
		t.Errorf("teardown serviceNames = %v, want [%s]", td.gotNames, computeCanonical)
	}

	// Success clears the maps and Get reports the wrapped sentinel.
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("expected proj-a removed from engaged set after successful teardown")
	}
	if _, err := p.Get(context.Background(), "proj-a"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("after teardown Get should wrap ErrClusterNotFound, got %v", err)
	}
}

func TestDisengage_DeletesCartesianOverServiceNames(t *testing.T) {
	rec := &recordingClient{}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical, storageCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	engageStub(p, "proj-a")

	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("disengage: %v", err)
	}

	// One DeleteAllOf per (GVK x serviceName) = 1 x 2.
	if len(rec.deletes) != 2 {
		t.Fatalf("expected 2 DeleteAllOf (1 GVK x 2 services), got %d: %+v", len(rec.deletes), rec.deletes)
	}
	got := map[string]bool{}
	for _, d := range rec.deletes {
		if d.gvk != locationBindingGVK {
			t.Errorf("unexpected delete gvk %v", d.gvk)
		}
		got[d.selector] = true
	}
	for _, want := range []string{
		labelServiceName + "=" + computeCanonical,
		labelServiceName + "=" + storageCanonical,
	} {
		if !got[want] {
			t.Errorf("missing DeleteAllOf for selector %q (got %v)", want, got)
		}
	}
}

func TestDisengage_TeardownErrorAbortsKeepsEngagedThenSucceeds(t *testing.T) {
	rec := &recordingClient{}
	boom := errors.New("teardown boom")
	td := &recordingTeardown{err: boom}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	clusterCtx := engageStub(p, "proj-a")
	td.watchCtx = func() context.Context { return clusterCtx }

	// Abort: the teardown fails.
	if err := p.disengage(context.Background(), "proj-a"); err == nil {
		t.Fatalf("expected disengage to return the teardown error")
	}
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Errorf("cluster must stay ENGAGED on teardown abort (retry marker)")
	}
	if _, ok := p.cancelFns["proj-a"]; !ok {
		t.Errorf("cancelFn must be retained on teardown abort")
	}
	// Cancel still happened before teardown — we never force past, but we also
	// never un-cancel.
	if clusterCtx.Err() == nil {
		t.Errorf("per-cluster ctx must be cancelled even on a failing teardown")
	}
	// ManagedResources delete ran before the failing teardown.
	if len(rec.deletes) != 1 {
		t.Errorf("expected ManagedResources delete to run before teardown, got %d", len(rec.deletes))
	}

	// Recover: teardown now succeeds; the next disengage clears the maps.
	td.err = nil
	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("disengage retry: %v", err)
	}
	if td.calls != 2 {
		t.Errorf("teardown must be retried on the next disengage, calls=%d want 2", td.calls)
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("expected cluster removed after successful teardown")
	}
	if _, err := p.Get(context.Background(), "proj-a"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("after success Get should wrap ErrClusterNotFound, got %v", err)
	}
}

func TestDisengage_DeleteErrorAbortsBeforeTeardowns(t *testing.T) {
	rec := &recordingClient{err: errors.New("delete boom")}
	td := &recordingTeardown{}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	engageStub(p, "proj-a")

	if err := p.disengage(context.Background(), "proj-a"); err == nil {
		t.Fatalf("expected disengage to surface the DeleteAllOf error")
	}
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Errorf("cluster must stay engaged when a ManagedResources delete fails")
	}
	// Teardowns must NOT run once the delete step has failed.
	if td.calls != 0 {
		t.Errorf("teardown must not run after a failed ManagedResources delete, calls=%d", td.calls)
	}
}

// A missing type in the consumer project (the GVK's CRD isn't installed) must be
// tolerated as "nothing to clean up", NOT an abort — otherwise disengage wedges
// in a permanent retry it can never satisfy.
func TestDisengage_ToleratesMissingType_NoMatchError(t *testing.T) {
	rec := &recordingClient{err: &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: locationBindingGVK.Group, Kind: locationBindingGVK.Kind},
		SearchedVersions: []string{locationBindingGVK.Version},
	}}
	// A Teardown is declared too: a missing managed-type CRD must NOT block the
	// teardown hooks — they still run.
	td := &recordingTeardown{}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	engageStub(p, "proj-a")

	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("a missing type (NoMatchError) must be tolerated as clean teardown, got %v", err)
	}
	// Teardown hooks still run despite the tolerated missing managed type.
	if td.calls != 1 {
		t.Errorf("Teardowns must still run when a managed type is missing, calls=%d want 1", td.calls)
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("expected proj-a removed after a tolerated missing-type teardown")
	}
	if _, err := p.Get(context.Background(), "proj-a"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("after tolerated teardown Get should wrap ErrClusterNotFound, got %v", err)
	}
}

// A NotFound from DeleteAllOf (collection already gone) is likewise tolerated.
func TestDisengage_ToleratesMissingType_NotFound(t *testing.T) {
	rec := &recordingClient{err: apierrors.NewNotFound(
		schema.GroupResource{Group: locationBindingGVK.Group, Resource: "locationbindings"}, "")}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	engageStub(p, "proj-a")

	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("a NotFound from DeleteAllOf must be tolerated, got %v", err)
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("expected proj-a removed after a tolerated NotFound teardown")
	}
}

func TestDisengage_TeardownIsIdempotent(t *testing.T) {
	rec := &recordingClient{} // DeleteAllOf returns nil: nothing to delete == no-op
	td := &recordingTeardown{}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)

	// Tear down, then engage again and tear down a second time: both clean.
	engageStub(p, "proj-a")
	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("disengage 1: %v", err)
	}
	engageStub(p, "proj-a")
	if err := p.disengage(context.Background(), "proj-a"); err != nil {
		t.Fatalf("disengage 2 (idempotent re-run): %v", err)
	}
	if len(rec.deletes) != 2 {
		t.Errorf("expected 2 DeleteAllOf across 2 teardowns, got %d", len(rec.deletes))
	}
	if td.calls != 2 {
		t.Errorf("teardown calls = %d, want 2", td.calls)
	}
}

// End-to-end through Reconcile: a teardown error must surface from Reconcile
// (driving requeue/backoff) and leave the cluster engaged; a later success
// disengages it.
func TestReconcile_TeardownErrorKeepsEngagedThenRecovers(t *testing.T) {
	consumer := newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive)
	providerClient := newProviderClient(t, newService(computeObject, computeCanonical), consumer)
	rec := &recordingClient{}
	td := &recordingTeardown{err: errors.New("boom")}
	p := newTestProvider(providerClient, []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)

	// Engage proj-a.
	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile (engage): %v", err)
	}
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Fatalf("precondition: proj-a should be engaged")
	}

	// Drop the consumer so proj-a leaves the active set; teardown will fail.
	if err := providerClient.Delete(context.Background(),
		&servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: consumer.Name}}); err != nil {
		t.Fatalf("delete consumer: %v", err)
	}
	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatalf("expected Reconcile to surface the teardown error (drives requeue/backoff)")
	}
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Errorf("cluster must stay engaged after a failed teardown")
	}

	// Teardown recovers; the next reconcile disengages proj-a.
	td.err = nil
	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile (recover): %v", err)
	}
	if _, ok := p.clusters["proj-a"]; ok {
		t.Errorf("expected proj-a disengaged after teardown success")
	}
}

// --- Phase 4: tearingDown Get sentinel + observability metrics ---------------

// readMetric snapshots a Prometheus metric's current value.
func readMetric(t *testing.T, m prometheus.Metric) *dto.Metric {
	t.Helper()
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		t.Fatalf("metric write: %v", err)
	}
	return &d
}

// A project whose teardown is failing stays tracked in p.clusters as a retry
// marker, but Get must report it as NOT engaged (wrapped sentinel) — its cache
// is cancelled, so handing the cluster back would only yield failing reads.
func TestGet_MidTeardownReportsSentinel(t *testing.T) {
	rec := &recordingClient{}
	td := &recordingTeardown{err: errors.New("teardown stuck")}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)
	engageStub(p, "proj-a")

	if err := p.disengage(context.Background(), "proj-a"); err == nil {
		t.Fatalf("expected the failing teardown to return an error")
	}
	// Still tracked as a retry marker...
	if _, ok := p.clusters["proj-a"]; !ok {
		t.Errorf("project must stay tracked in p.clusters during a failing teardown")
	}
	// ...but Get reports the wrapped sentinel because it is mid-teardown.
	if _, err := p.Get(context.Background(), "proj-a"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Errorf("Get must wrap ErrClusterNotFound for a mid-teardown project, got %v", err)
	}
}

// The engaged-clusters gauge tracks the live count: it rises on engage and
// returns to zero on disengage.
func TestMetrics_EngagedClustersGauge(t *testing.T) {
	consumer := newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive)
	providerClient := newProviderClient(t, newService(computeObject, computeCanonical), consumer)
	p := newTestProvider(providerClient, []string{computeCanonical}, withMCMgr(newFakeMCMgr()))

	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile (engage): %v", err)
	}
	if got := readMetric(t, engagedClusters).GetGauge().GetValue(); got != 1 {
		t.Errorf("engaged-clusters gauge = %v, want 1 after engage", got)
	}

	// Drop the consumer so proj-a leaves the active set; disengage -> gauge back to 0.
	if err := providerClient.Delete(context.Background(),
		&servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: consumer.Name}}); err != nil {
		t.Fatalf("delete consumer: %v", err)
	}
	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile (disengage): %v", err)
	}
	if got := readMetric(t, engagedClusters).GetGauge().GetValue(); got != 0 {
		t.Errorf("engaged-clusters gauge = %v, want 0 after disengage", got)
	}
}

// Each failing teardown increments the per-project teardown-failure counter
// (alert-only; retried with backoff). A unique project label isolates the
// series from any other test.
func TestMetrics_TeardownFailureCounter(t *testing.T) {
	const proj = "metrics-teardown-proj"
	rec := &recordingClient{}
	td := &recordingTeardown{err: errors.New("stuck")}
	p := newTestProvider(newProviderClient(t), []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withTeardowns(td),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)

	before := readMetric(t, teardownFailuresTotal.WithLabelValues(proj)).GetCounter().GetValue()

	engageStub(p, proj)
	if err := p.disengage(context.Background(), proj); err == nil {
		t.Fatalf("expected failing teardown (1)")
	}
	if got := readMetric(t, teardownFailuresTotal.WithLabelValues(proj)).GetCounter().GetValue(); got != before+1 {
		t.Errorf("teardown-failure counter = %v, want %v after one failure", got, before+1)
	}

	// A second failing pass increments again (retried; never force-cancelled).
	engageStub(p, proj)
	if err := p.disengage(context.Background(), proj); err == nil {
		t.Fatalf("expected failing teardown (2)")
	}
	if got := readMetric(t, teardownFailuresTotal.WithLabelValues(proj)).GetCounter().GetValue(); got != before+2 {
		t.Errorf("teardown-failure counter = %v, want %v after two failures", got, before+2)
	}
}

// --- Phase 4: restart-safety (resync teardown of a consumer revoked while down)

// Restart-safety: a consumer revoked (Denied) — or deleted — while the operator
// was DOWN leaves projected resources behind. After a restart p.clusters is
// empty, so the project is in neither the engage set nor (pre-fix) the disengage
// set, and consumer-side owner-ref GC never fires because the consumer object
// still exists. The resync recompute must put it in the REVOKED set and tear it
// down via a fresh direct client WITHOUT ever engaging it.
func TestReconcile_RestartTearsDownRevokedConsumerWithoutEngaging(t *testing.T) {
	// Denied consumer for proj-a; p.clusters starts EMPTY (fresh process).
	providerClient := newProviderClient(t,
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseDenied),
	)
	rec := &recordingClient{}
	mcMgr := newFakeMCMgr()
	p := newTestProvider(providerClient, []string{computeCanonical},
		withMCMgr(mcMgr),
		withManagedResources(locationBindingGVK),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)

	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Teardown ran for proj-a, scoped to the canonical label, via the direct client.
	if len(rec.deletes) != 1 {
		t.Fatalf("expected 1 DeleteAllOf for the revoked project's projected resources, got %d", len(rec.deletes))
	}
	if rec.deletes[0].gvk != locationBindingGVK {
		t.Errorf("delete gvk = %v, want %v", rec.deletes[0].gvk, locationBindingGVK)
	}
	if want := labelServiceName + "=" + computeCanonical; rec.deletes[0].selector != want {
		t.Errorf("delete selector = %q, want %q", rec.deletes[0].selector, want)
	}
	// The project was NEVER engaged — no cluster created, no Engage call.
	if len(p.clusters) != 0 {
		t.Errorf("revoked-while-down project must not be engaged, clusters=%v", p.clusters)
	}
	if _, ok := mcMgr.engaged["proj-a"]; ok {
		t.Errorf("mcMgr.Engage must not be called for a revoked-while-down project")
	}
}

// A PendingApproval-only project was never Active, so nothing was projected and
// the resync must NOT sweep it (no wasteful teardown over every pending request).
func TestReconcile_PendingOnlyProjectIsNotTornDown(t *testing.T) {
	providerClient := newProviderClient(t,
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhasePendingApproval),
	)
	rec := &recordingClient{}
	p := newTestProvider(providerClient, []string{computeCanonical},
		withMCMgr(newFakeMCMgr()),
		withManagedResources(locationBindingGVK),
		withNewClient(func(*rest.Config, client.Options) (client.Client, error) { return rec, nil }),
	)

	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.deletes) != 0 {
		t.Errorf("a pending-only project must not be torn down, got %d DeleteAllOf", len(rec.deletes))
	}
	if len(p.clusters) != 0 {
		t.Errorf("a pending-only project must not be engaged, clusters=%v", p.clusters)
	}
}
