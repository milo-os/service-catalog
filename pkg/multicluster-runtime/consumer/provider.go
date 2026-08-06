// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

var _ multicluster.Provider = &Provider{}
var _ multicluster.ProviderRunnable = &Provider{}

const (
	// serviceRefNameField is the cache field index on
	// ServiceConsumer.spec.serviceRef.name, used to list a service's consumers
	// without scanning every ServiceConsumer in the provider project.
	serviceRefNameField = "spec.serviceRef.name"

	// providerTeardownFinalizer is placed on a ServiceConsumer in the provider
	// project while this operator may hold projected resources for it, so the
	// consumer's deletion cannot complete until this operator has confirmed
	// teardown. The provider ADDS it on an active/denied consumer and REMOVES it
	// only after teardown of that project succeeds — it never DELETES the
	// ServiceConsumer (the consumer-side ServiceEntitlement cascade owns
	// deletion); the finalizer only gates that deletion.
	//
	// Each provider owns the ServiceConsumers for its own ServiceNames, so a
	// single finalizer per object gates the one operator that could have
	// projected resources for it. A different operator's services map to
	// different ServiceConsumer objects, each independently gated.
	providerTeardownFinalizer = "services.miloapis.com/provider-teardown"

	// pausedConditionFieldManager is the field manager recorded in
	// ServiceConsumer's managedFields when this provider patches the Paused
	// condition, distinguishing it from the platform's own
	// suspendedConditionFieldManager entry for the Suspended condition.
	// Scoping the write to just this field comes from the merge patch itself
	// (computed from a before/after diff in reconcileSuspension) —
	// FieldOwner only labels provenance, it doesn't limit what the patch
	// touches.
	pausedConditionFieldManager = "services-consumer-paused-condition"

	// mcMgrUnboundRequeue is how long to requeue when Start has not yet bound
	// the Aware. With two independently-started managers this window is wider
	// than Milo's single-manager case, so the guard is retained.
	mcMgrUnboundRequeue = 2 * time.Second

	// cacheSyncTimeout bounds WaitForCacheSync for a single engagement so one
	// slow or unreachable consumer project cannot head-of-line-block other
	// engagements under MaxConcurrentReconciles: 1.
	cacheSyncTimeout = 2 * time.Minute

	// readinessPollInterval is how often WaitProviderProjectReady polls the
	// provider Project's Ready condition.
	readinessPollInterval = 5 * time.Second
)

// projectGVK is the resourcemanager Project type, read as unstructured for the
// readiness gate (mirrors how the catalog reads Location unstructured).
var projectGVK = schema.GroupVersionKind{
	Group:   "resourcemanager.miloapis.com",
	Version: "v1alpha1",
	Kind:    "Project",
}

// index records a field index to (re)apply to every engaged consumer cluster.
type index struct {
	object       client.Object
	field        string
	extractValue client.IndexerFunc
}

// Provider engages consumer projects as cluster.Clusters while they have an
// active ServiceConsumer for one of Options.ServiceNames. It is both a
// controller-runtime reconciler — registered on the caller-supplied providerMgr,
// watching ServiceConsumer in the provider project — and a
// sigs.k8s.io/multicluster-runtime Provider whose engaged clusters are consumer
// projects.
type Provider struct {
	opts               Options
	log                logr.Logger
	rootClient         client.Client
	providerClient     client.Client
	providerRestConfig *rest.Config
	resyncInterval     time.Duration
	newCluster         func(*rest.Config, ...cluster.Option) (cluster.Cluster, error)
	newClient          func(*rest.Config, client.Options) (client.Client, error)

	// serviceNames is the canonical Service.spec.serviceName set, for O(1) match.
	serviceNames map[string]struct{}

	lock      sync.Mutex
	aware     multicluster.Aware
	clusters  map[string]cluster.Cluster
	cancelFns map[string]context.CancelFunc
	indexers  []index

	// tearingDown holds projects whose disengage has started (context cancelled)
	// but whose teardown has not yet succeeded. Such a project is still in
	// clusters/cancelFns as a retry marker, but its cache is dead, so Get reports
	// it as not-engaged (the %w sentinel) rather than handing back a dead cluster.
	tearingDown map[string]struct{}
}

// New registers the provider as a reconciler on providerMgr. providerMgr MUST
// already be pointed at the PROVIDER project: it is the single source of truth
// for which project hosts the ServiceConsumer objects. The provider re-addresses
// that manager's rest.Config to the matching consumer project when engaging each
// one — there is no separate ProviderProject field that could disagree.
func New(providerMgr manager.Manager, opts Options) (*Provider, error) {
	if opts.RootClient == nil {
		return nil, fmt.Errorf("consumer.Options.RootClient must be set")
	}
	if len(opts.ServiceNames) == 0 {
		return nil, fmt.Errorf("consumer.Options.ServiceNames must be non-empty")
	}
	if opts.newCluster == nil {
		opts.newCluster = cluster.New
	}
	if opts.newClient == nil {
		opts.newClient = client.New
	}
	if opts.ResyncInterval == 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}

	serviceNames := make(map[string]struct{}, len(opts.ServiceNames))
	for _, n := range opts.ServiceNames {
		serviceNames[n] = struct{}{}
	}

	p := &Provider{
		opts:               opts,
		log:                log.Log.WithName("consumer-provider"),
		rootClient:         opts.RootClient,
		providerClient:     providerMgr.GetClient(),
		providerRestConfig: providerMgr.GetConfig(),
		resyncInterval:     opts.ResyncInterval,
		newCluster:         opts.newCluster,
		newClient:          opts.newClient,
		serviceNames:       serviceNames,
		clusters:           map[string]cluster.Cluster{},
		cancelFns:          map[string]context.CancelFunc{},
		tearingDown:        map[string]struct{}{},
	}

	// Field index on spec.serviceRef.name so computeActiveSet can list a
	// service's consumers by object name instead of scanning all of them.
	if err := providerMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&servicesv1alpha1.ServiceConsumer{},
		serviceRefNameField,
		func(obj client.Object) []string {
			sc, ok := obj.(*servicesv1alpha1.ServiceConsumer)
			if !ok {
				return nil
			}
			return []string{sc.Spec.ServiceRef.Name}
		},
	); err != nil {
		return nil, fmt.Errorf("failed to index ServiceConsumer field %q: %w", serviceRefNameField, err)
	}

	if err := builder.ControllerManagedBy(providerMgr).
		For(&servicesv1alpha1.ServiceConsumer{}, builder.WithPredicates(consumerEventPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("consumer-serviceconsumer").
		Complete(p); err != nil {
		return nil, fmt.Errorf("failed to create consumer provider controller: %w", err)
	}

	return p, nil
}

// consumerEventPredicate filters out ServiceConsumer updates that cannot change
// the engagement set. Scoping to the operator's OWN services requires resolving
// each consumer's serviceRef.name to its canonical Service.spec.serviceName,
// which needs a client read; that authoritative filtering happens in
// computeActiveSet. This predicate is purely churn reduction.
func consumerEventPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldC, ok1 := e.ObjectOld.(*servicesv1alpha1.ServiceConsumer)
			newC, ok2 := e.ObjectNew.(*servicesv1alpha1.ServiceConsumer)
			if !ok1 || !ok2 {
				return true
			}
			oldSusp := apimeta.IsStatusConditionTrue(oldC.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended)
			newSusp := apimeta.IsStatusConditionTrue(newC.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended)
			return oldC.Status.Phase != newC.Status.Phase ||
				oldC.Spec.ServiceRef.Name != newC.Spec.ServiceRef.Name ||
				oldC.Spec.ConsumerProjectRef.Name != newC.Spec.ConsumerProjectRef.Name ||
				oldC.DeletionTimestamp.IsZero() != newC.DeletionTimestamp.IsZero() ||
				oldSusp != newSusp
		},
	}
}

// Get returns the engaged cluster for consumerProject, or
// multicluster.ErrClusterNotFound wrapped with %w when it is not engaged so the
// framework's ClusterNotFoundWrapper drops stale requeues silently. A project
// that is mid-teardown (in tearingDown) reports not-engaged even though it is
// still tracked as a retry marker — its cache is cancelled, so handing back the
// cluster would only yield failing reads.
func (p *Provider) Get(_ context.Context, name multicluster.ClusterName) (cluster.Cluster, error) {
	consumerProject := string(name)
	p.lock.Lock()
	defer p.lock.Unlock()
	if _, tearing := p.tearingDown[consumerProject]; !tearing {
		if cl, ok := p.clusters[consumerProject]; ok {
			return cl, nil
		}
	}
	return nil, fmt.Errorf("consumer project %q not engaged: %w", consumerProject, multicluster.ErrClusterNotFound)
}

// Start implements multicluster.ProviderRunnable. It binds the Aware and blocks
// until ctx is done. The multicluster manager calls this automatically when it
// detects the ProviderRunnable interface on Start.
func (p *Provider) Start(ctx context.Context, aware multicluster.Aware) error {
	p.log.Info("starting consumer provider")
	p.lock.Lock()
	p.aware = aware
	p.lock.Unlock()

	<-ctx.Done()
	return ctx.Err()
}

// IndexField forwards a field index to every engaged consumer cluster, current
// and future.
func (p *Provider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.indexers = append(p.indexers, index{object: obj, field: field, extractValue: extractValue})
	for name, cl := range p.clusters {
		if err := cl.GetCache().IndexField(ctx, obj, field, extractValue); err != nil {
			return fmt.Errorf("failed to index field %q on consumer project %q: %w", field, name, err)
		}
	}
	return nil
}

// Reconcile recomputes the active consumer-project set from a list and engages
// or disengages on the delta against the currently-engaged set. The triggering
// request is intentionally ignored: a delete event for a hash-named
// ServiceConsumer carries only the hash key, so correctness comes from the full
// recompute, not from the individual event.
func (p *Provider) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	p.lock.Lock()
	aware := p.aware
	p.lock.Unlock()

	// Start binds aware after the manager starts; with two independently-started
	// managers the unbound window is wide, so requeue rather than nil-panic.
	if aware == nil {
		p.log.Info("multicluster manager not yet bound, requeueing")
		return ctrl.Result{RequeueAfter: mcMgrUnboundRequeue}, nil
	}

	active, revoked, byProject, err := p.computeActiveSet(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Gate deletion on teardown: stamp the provider-teardown finalizer on every
	// consumer that may hold projected resources (Active or Denied, not yet being
	// deleted) BEFORE it can be deleted. Removal happens in disengage once
	// teardown for the project succeeds. A no-op for providers that declare no
	// cleanup, so their consumers are never gated.
	if err := p.ensureTeardownFinalizers(ctx, byProject); err != nil {
		return ctrl.Result{}, err
	}

	p.lock.Lock()
	engaged := make(map[string]struct{}, len(p.clusters))
	for name := range p.clusters {
		engaged[name] = struct{}{}
	}
	p.lock.Unlock()

	// Engage newly-active consumer projects.
	for project := range active {
		if _, ok := engaged[project]; ok {
			continue
		}
		if err := p.engage(ctx, project); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to engage consumer project %q: %w", project, err)
		}
	}

	// Reconcile suspend/resume hooks for active consumer projects.
	for project := range active {
		if err := p.reconcileSuspension(ctx, project, byProject[project]); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile suspension for consumer project %q: %w", project, err)
		}
	}

	// Disengage + tear down projects that are no longer active. Candidates are
	// the currently-engaged projects PLUS any project with a revoked (Denied) or
	// being-deleted consumer. The latter catches a consumer revoked or deleted
	// while the operator was DOWN, whose projected resources would otherwise leak
	// after a restart: the project never re-engages (its consumer isn't active)
	// and consumer-side owner-ref GC never fires (the consumer object still
	// exists). disengage is safe on a never-engaged project — there is no
	// cancelFn to call, the map deletes are no-ops, and teardown runs via a fresh
	// non-cached direct client without ever engaging the cluster.
	candidates := make(map[string]struct{}, len(engaged)+len(revoked))
	for project := range engaged {
		candidates[project] = struct{}{}
	}
	for project := range revoked {
		candidates[project] = struct{}{}
	}
	for project := range candidates {
		if _, ok := active[project]; ok {
			continue
		}
		if err := p.disengage(ctx, project, byProject[project]); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to disengage consumer project %q: %w", project, err)
		}
	}

	// Periodic full resync covers consumers removed while the operator was down
	// and provider-side gate changes the watch cannot observe.
	return ctrl.Result{RequeueAfter: p.resyncInterval}, nil
}

// computeActiveSet lists the operator's Services in the provider project to
// resolve canonical names to Service object names, then lists each service's
// ServiceConsumers (via the field index) and buckets their consumer projects:
//
//   - active: at least one Active, not-deleting consumer → engage.
//   - revoked: at least one Denied or being-deleted consumer → a project that
//     may hold projected resources but whose consumer is no longer entitled.
//     These are torn down even when not currently engaged, which is how a
//     consumer revoked (Active→Denied) or deleted while the operator was DOWN
//     gets cleaned up after a restart — its object still exists, so consumer-side
//     owner-ref GC never reclaims the projected resources, and the project would
//     otherwise be in neither the engage nor the disengage set.
//
// PendingApproval-only projects appear in neither set: a pending consumer was
// never Active, so nothing was ever projected and there is nothing to tear down
// — avoiding a no-op teardown sweep over every pending request each resync.
//
// revoked MAY transiently include a project that is also in active — one with an
// Active consumer for one service and a Denied/deleting one for a sibling
// service. That is intentional: Reconcile's (engaged ∪ revoked) − active
// subtraction is the SOLE teardown authority, so a project is never torn down
// while any sibling service still keeps it active.
//
// byProject buckets every matched ServiceConsumer object by its consumer
// project, so the caller can stamp/remove the provider-teardown finalizer on the
// exact objects without re-listing.
func (p *Provider) computeActiveSet(ctx context.Context) (active, revoked map[string]struct{}, byProject map[string][]servicesv1alpha1.ServiceConsumer, err error) {
	var services servicesv1alpha1.ServiceList
	if err := p.rootClient.List(ctx, &services); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list Services: %w", err)
	}

	// Resolve our canonical service names to Service object names. serviceRef.name
	// on a ServiceConsumer is the object name, so this is the join key that lets
	// the engagement match-key equal the cleanup label (canonical serviceName).
	objectNames := make([]string, 0, len(p.serviceNames))
	for i := range services.Items {
		svc := &services.Items[i]
		if _, ok := p.serviceNames[svc.Spec.ServiceName]; ok {
			objectNames = append(objectNames, svc.Name)
		}
	}

	active = map[string]struct{}{}
	revoked = map[string]struct{}{}
	byProject = map[string][]servicesv1alpha1.ServiceConsumer{}
	for _, objectName := range objectNames {
		var consumers servicesv1alpha1.ServiceConsumerList
		if err := p.providerClient.List(ctx, &consumers,
			client.MatchingFields{serviceRefNameField: objectName},
		); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to list ServiceConsumers for service %q: %w", objectName, err)
		}
		for i := range consumers.Items {
			sc := &consumers.Items[i]
			project := sc.Spec.ConsumerProjectRef.Name
			if project == "" {
				continue
			}
			byProject[project] = append(byProject[project], *sc)
			switch {
			case !sc.DeletionTimestamp.IsZero():
				revoked[project] = struct{}{}
			case sc.Status.Phase == servicesv1alpha1.ConsumerPhaseActive:
				active[project] = struct{}{}
			case sc.Status.Phase == servicesv1alpha1.ConsumerPhaseDenied:
				revoked[project] = struct{}{}
			}
		}
	}
	return active, revoked, byProject, nil
}

// ensureTeardownFinalizers stamps providerTeardownFinalizer on every consumer
// that could hold projected resources — Active or Denied, and not already being
// deleted — so a subsequent delete blocks on teardown. It is a no-op when the
// operator declared no cleanup (neither ManagedResources nor Teardowns): such an
// operator projects nothing, so gating its consumers would only strand
// deletions. Adding the finalizer while the object still exists is what makes
// the gate reliable; once a deletionTimestamp is set with no finalizer present
// the object is already gone.
func (p *Provider) ensureTeardownFinalizers(ctx context.Context, byProject map[string][]servicesv1alpha1.ServiceConsumer) error {
	if len(p.opts.ManagedResources) == 0 && len(p.opts.Teardowns) == 0 {
		return nil
	}
	for _, consumers := range byProject {
		for i := range consumers {
			sc := &consumers[i]
			if !sc.DeletionTimestamp.IsZero() {
				continue
			}
			switch sc.Status.Phase {
			case servicesv1alpha1.ConsumerPhaseActive, servicesv1alpha1.ConsumerPhaseDenied:
			default:
				continue
			}
			if controllerutil.ContainsFinalizer(sc, providerTeardownFinalizer) {
				continue
			}
			controllerutil.AddFinalizer(sc, providerTeardownFinalizer)
			if err := p.providerClient.Update(ctx, sc); err != nil {
				return fmt.Errorf("failed to add teardown finalizer to ServiceConsumer %q: %w", sc.Name, err)
			}
		}
	}
	return nil
}

// releaseTeardownFinalizers removes providerTeardownFinalizer from every
// consumer in the slice that is being deleted, unblocking its garbage
// collection. It runs only after teardown for the project has succeeded, so the
// resources this operator created are already gone. It is idempotent: a consumer
// without the finalizer, or already gone (NotFound), is skipped. A conflict or
// other error is returned so disengage keeps the project tracked and the next
// reconcile retries against a fresh read.
func (p *Provider) releaseTeardownFinalizers(ctx context.Context, consumers []servicesv1alpha1.ServiceConsumer) error {
	for i := range consumers {
		sc := &consumers[i]
		if sc.DeletionTimestamp.IsZero() {
			continue
		}
		if !controllerutil.ContainsFinalizer(sc, providerTeardownFinalizer) {
			continue
		}
		controllerutil.RemoveFinalizer(sc, providerTeardownFinalizer)
		if err := p.providerClient.Update(ctx, sc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to remove teardown finalizer from ServiceConsumer %q: %w", sc.Name, err)
		}
	}
	return nil
}

// engage builds a cluster.Cluster for the consumer project (re-addressing the
// provider rest.Config to its control plane), starts its cache, waits for a
// bounded sync, and engages it with the multicluster manager. Mirrors Milo's
// engage path.
func (p *Provider) engage(ctx context.Context, consumerProject string) error {
	cfg, err := p.consumerRestConfig(consumerProject)
	if err != nil {
		return err
	}

	cl, err := p.newCluster(cfg, p.opts.ClusterOptions...)
	if err != nil {
		return fmt.Errorf("failed to create cluster: %w", err)
	}

	p.lock.Lock()
	indexers := append([]index(nil), p.indexers...)
	p.lock.Unlock()
	for _, idx := range indexers {
		if err := cl.GetCache().IndexField(ctx, idx.object, idx.field, idx.extractValue); err != nil {
			return fmt.Errorf("failed to index field %q: %w", idx.field, err)
		}
	}

	clusterCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := cl.Start(clusterCtx); err != nil {
			p.log.Error(err, "consumer cluster cache start failed", "consumerProject", consumerProject)
		}
	}()

	syncCtx, syncCancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer syncCancel()
	if !cl.GetCache().WaitForCacheSync(syncCtx) {
		cancel()
		return fmt.Errorf("cache sync failed or timed out for consumer project %q", consumerProject)
	}

	p.lock.Lock()
	defer p.lock.Unlock()
	if err := p.aware.Engage(clusterCtx, multicluster.ClusterName(consumerProject), cl); err != nil {
		cancel()
		return fmt.Errorf("failed to engage with multicluster manager: %w", err)
	}
	p.clusters[consumerProject] = cl
	p.cancelFns[consumerProject] = cancel
	setEngagedClusters(len(p.clusters))
	p.log.Info("engaged consumer project", "consumerProject", consumerProject, "endpoint", cfg.Host)
	return nil
}

// disengage drops a consumer project using cancel-before-teardown:
//
//  1. Cancel the per-cluster context FIRST. This stops the operator's
//     reconcilers for that cluster before teardown deletes anything — otherwise a
//     still-queued reconcile could re-create a resource we just deleted. cancel()
//     is idempotent, so calling it again on a teardown retry is a harmless no-op.
//  2. Run teardown (delete ManagedResources label-scoped, then Teardowns) via a
//     fresh NON-CACHED direct client — the cache is being torn down here.
//  3. On teardown error, return WITHOUT removing the maps: the cluster stays
//     tracked and Reconcile's engaged-vs-active diff re-invokes disengage (a
//     fresh direct client each pass) until it succeeds — requeue with backoff,
//     never force past a failing teardown.
//  4. Only on full teardown success: remove from the maps.
//
// Once the context is cancelled (step 1) the cluster's cache is dead, so the
// project is marked tearingDown from that point: Get reports it as not-engaged
// even while it lingers in the maps as a retry marker, and the mark is cleared
// only when it leaves the maps on success.
//
// consumers is the set of this project's matched ServiceConsumer objects (as of
// this reconcile). After teardown succeeds, the provider-teardown finalizer is
// removed from any of them that are being deleted, which is what lets the
// consumer's deletion — and, through the ServiceEntitlement handshake, the
// project's deletion — finally complete.
func (p *Provider) disengage(ctx context.Context, consumerProject string, consumers []servicesv1alpha1.ServiceConsumer) error {
	p.lock.Lock()
	cancel, ok := p.cancelFns[consumerProject]
	if _, engaged := p.clusters[consumerProject]; engaged {
		p.tearingDown[consumerProject] = struct{}{}
	}
	p.lock.Unlock()
	if ok {
		cancel()
	}

	if err := p.teardownConsumer(ctx, consumerProject); err != nil {
		// Cluster stays in p.clusters/p.cancelFns (and tearingDown) as the retry
		// marker; the surfaced error drives Reconcile's requeue-with-backoff.
		p.recordTeardownFailure(consumerProject)
		return err
	}

	// Teardown succeeded: the resources this operator created for the project are
	// gone, so it is safe to drop the deletion gate on any being-deleted consumer.
	// A failure here keeps the cluster tracked (same retry contract as teardown),
	// since teardown is idempotent on the next pass.
	if err := p.releaseTeardownFinalizers(ctx, consumers); err != nil {
		return err
	}

	p.lock.Lock()
	delete(p.clusters, consumerProject)
	delete(p.cancelFns, consumerProject)
	delete(p.tearingDown, consumerProject)
	engaged := len(p.clusters)
	p.lock.Unlock()
	setEngagedClusters(engaged)

	p.log.Info("disengaged consumer project", "consumerProject", consumerProject)
	return nil
}

// consumerRestConfig re-addresses the provider rest.Config to the consumer
// project's control plane.
func (p *Provider) consumerRestConfig(consumerProject string) (*rest.Config, error) {
	return ProjectRestConfig(p.providerRestConfig, consumerProject)
}

// ProjectRestConfig copies base and re-addresses its host path to the given
// Milo project's control plane
// (/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane).
// Callers building a providerMgr pointed at the provider project can reuse this
// instead of hand-rolling the host rewrite; the provider uses it internally to
// re-address each engaged consumer project.
func ProjectRestConfig(base *rest.Config, project string) (*rest.Config, error) {
	if base == nil {
		return nil, fmt.Errorf("base rest.Config must not be nil")
	}
	if project == "" {
		return nil, fmt.Errorf("project must not be empty")
	}
	cfg := rest.CopyConfig(base)
	apiHost, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host %q: %w", cfg.Host, err)
	}
	apiHost.Path = fmt.Sprintf("/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane", project)
	cfg.Host = apiHost.String()
	return cfg, nil
}

// WaitProviderProjectReady blocks until the provider Project reports
// status.conditions[Ready]=True, or ctx is cancelled. It reads the Project once
// per poll from the base Milo cluster (mirroring Milo's readiness gate) so a
// providerMgr pointed at a not-yet-ready project does not crash-loop on Start.
func WaitProviderProjectReady(ctx context.Context, baseCfg *rest.Config, providerProject string) error {
	cl, err := client.New(baseCfg, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to build client for provider-project readiness: %w", err)
	}

	return wait.PollUntilContextCancel(ctx, readinessPollInterval, true, func(ctx context.Context) (bool, error) {
		project := &unstructured.Unstructured{}
		project.SetGroupVersionKind(projectGVK)
		if err := cl.Get(ctx, types.NamespacedName{Name: providerProject}, project); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			// Transient (API not reachable yet, RBAC propagation): keep polling.
			return false, nil
		}
		conditions, err := extractConditions(project.Object)
		if err != nil {
			return false, nil
		}
		return apimeta.IsStatusConditionTrue(conditions, "Ready"), nil
	})
}

// extractConditions reads status.conditions off an unstructured object into
// typed metav1.Conditions.
func extractConditions(obj map[string]any) ([]metav1.Condition, error) {
	raw, ok, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed reading status.conditions: %w", err)
	}
	if !ok {
		return nil, nil
	}
	wrapped := map[string]any{"conditions": raw}
	var typed struct {
		Conditions []metav1.Condition `json:"conditions"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(wrapped, &typed); err != nil {
		return nil, fmt.Errorf("failed converting unstructured conditions: %w", err)
	}
	return typed.Conditions, nil
}

// reconcileSuspension checks each active consumer for the project and runs
// Suspend/Resume hooks if their status has transitioned.
func (p *Provider) reconcileSuspension(ctx context.Context, project string, consumers []servicesv1alpha1.ServiceConsumer) error {
	var direct client.Client
	getDirect := func() (client.Client, error) {
		if direct != nil {
			return direct, nil
		}
		cfg, err := p.consumerRestConfig(project)
		if err != nil {
			return nil, err
		}
		cl, err := p.newClient(cfg, client.Options{Scheme: p.opts.Scheme})
		if err != nil {
			return nil, fmt.Errorf("failed to build direct client for project %q: %w", project, err)
		}
		direct = cl
		return direct, nil
	}

	for i := range consumers {
		sc := &consumers[i]
		if sc.DeletionTimestamp.IsZero() && sc.Status.Phase == servicesv1alpha1.ConsumerPhaseActive {
			// signal is the platform's own Suspended condition — the inbound
			// instruction to pause or resume. It is read-only here: writing
			// to it would collide with the platform's rollup and erase the
			// specific reason/message it carries.
			signal := apimeta.FindStatusCondition(sc.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended)
			if signal == nil {
				continue
			}
			signalSuspended := signal.Status == metav1.ConditionTrue
			confirmedPaused := apimeta.IsStatusConditionTrue(sc.Status.Conditions, servicesv1alpha1.ConditionTypePaused)
			if signalSuspended == confirmedPaused {
				// Already caught up with the signal; nothing to run.
				continue
			}

			cl, err := getDirect()
			if err != nil {
				return err
			}

			p.log.Info("suspension signal mismatch, dispatching hooks",
				"consumerProject", project, "consumer", sc.Name,
				"signalSuspended", signalSuspended, "confirmedPaused", confirmedPaused)

			before := sc.DeepCopy()
			if signalSuspended {
				if err := p.runSuspends(ctx, cl, project); err != nil {
					return err
				}
				apimeta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
					Type:               servicesv1alpha1.ConditionTypePaused,
					Status:             metav1.ConditionTrue,
					Reason:             servicesv1alpha1.ReasonPaused,
					Message:            "Suspend hooks have run; this provider's resources for the consumer are paused.",
					ObservedGeneration: sc.Generation,
				})
			} else {
				if err := p.runResumes(ctx, cl, project); err != nil {
					return err
				}
				apimeta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
					Type:               servicesv1alpha1.ConditionTypePaused,
					Status:             metav1.ConditionFalse,
					Reason:             servicesv1alpha1.ReasonActive,
					Message:            "Resume hooks have run; this provider's resources for the consumer are active.",
					ObservedGeneration: sc.Generation,
				})
			}
			if err := p.providerClient.Status().Patch(ctx, sc, client.MergeFrom(before), client.FieldOwner(pausedConditionFieldManager)); err != nil {
				return fmt.Errorf("failed to patch ServiceConsumer %q Paused condition: %w", sc.Name, err)
			}

			p.log.Info("suspension hooks completed, Paused condition updated",
				"consumerProject", project, "consumer", sc.Name, "paused", signalSuspended)
		}
	}
	return nil
}
