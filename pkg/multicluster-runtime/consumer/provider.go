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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

var _ multicluster.Provider = &Provider{}

const (
	// serviceRefNameField is the cache field index on
	// ServiceConsumer.spec.serviceRef.name, used to list a service's consumers
	// without scanning every ServiceConsumer in the provider project.
	serviceRefNameField = "spec.serviceRef.name"

	// mcMgrUnboundRequeue is how long to requeue when Run has not yet bound the
	// multicluster manager. With two independently-started managers this window
	// is wider than Milo's single-manager case, so the guard is retained.
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
	providerClient     client.Client
	providerRestConfig *rest.Config
	resyncInterval     time.Duration
	newCluster         func(*rest.Config, ...cluster.Option) (cluster.Cluster, error)
	newClient          func(*rest.Config, client.Options) (client.Client, error)

	// serviceNames is the canonical Service.spec.serviceName set, for O(1) match.
	serviceNames map[string]struct{}

	lock      sync.Mutex
	mcMgr     mcmanager.Manager
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
			return oldC.Status.Phase != newC.Status.Phase ||
				oldC.Spec.ServiceRef.Name != newC.Spec.ServiceRef.Name ||
				oldC.Spec.ConsumerProjectRef.Name != newC.Spec.ConsumerProjectRef.Name ||
				oldC.DeletionTimestamp.IsZero() != newC.DeletionTimestamp.IsZero()
		},
	}
}

// Get returns the engaged cluster for consumerProject, or
// multicluster.ErrClusterNotFound wrapped with %w when it is not engaged so the
// framework's ClusterNotFoundWrapper drops stale requeues silently. A project
// that is mid-teardown (in tearingDown) reports not-engaged even though it is
// still tracked as a retry marker — its cache is cancelled, so handing back the
// cluster would only yield failing reads.
func (p *Provider) Get(_ context.Context, consumerProject string) (cluster.Cluster, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if _, tearing := p.tearingDown[consumerProject]; !tearing {
		if cl, ok := p.clusters[consumerProject]; ok {
			return cl, nil
		}
	}
	return nil, fmt.Errorf("consumer project %q not engaged: %w", consumerProject, multicluster.ErrClusterNotFound)
}

// Run binds the consumer multicluster manager to the provider, then blocks until
// ctx is done. Launch it in a goroutine alongside providerMgr.Start and
// consumerMcMgr.Start.
func (p *Provider) Run(ctx context.Context, mcMgr mcmanager.Manager) error {
	p.log.Info("starting consumer provider")
	p.lock.Lock()
	p.mcMgr = mcMgr
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
	mcMgr := p.mcMgr
	p.lock.Unlock()

	// Run binds mcMgr after the manager starts; with two independently-started
	// managers the unbound window is wide, so requeue rather than nil-panic.
	if mcMgr == nil {
		p.log.Info("multicluster manager not yet bound, requeueing")
		return ctrl.Result{RequeueAfter: mcMgrUnboundRequeue}, nil
	}

	active, revoked, err := p.computeActiveSet(ctx)
	if err != nil {
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
		if err := p.disengage(ctx, project); err != nil {
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
func (p *Provider) computeActiveSet(ctx context.Context) (active, revoked map[string]struct{}, err error) {
	var services servicesv1alpha1.ServiceList
	if err := p.providerClient.List(ctx, &services); err != nil {
		return nil, nil, fmt.Errorf("failed to list Services in provider project: %w", err)
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
	for _, objectName := range objectNames {
		var consumers servicesv1alpha1.ServiceConsumerList
		if err := p.providerClient.List(ctx, &consumers,
			client.MatchingFields{serviceRefNameField: objectName},
		); err != nil {
			return nil, nil, fmt.Errorf("failed to list ServiceConsumers for service %q: %w", objectName, err)
		}
		for i := range consumers.Items {
			sc := &consumers.Items[i]
			project := sc.Spec.ConsumerProjectRef.Name
			if project == "" {
				continue
			}
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
	return active, revoked, nil
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
	if err := p.mcMgr.Engage(clusterCtx, consumerProject, cl); err != nil {
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
func (p *Provider) disengage(ctx context.Context, consumerProject string) error {
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
