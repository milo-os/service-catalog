// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// serviceAvailabilityMirrorGVK identifies the mirrored copy of a platform
// ServiceAvailability written into an entitled project. It is the same Kind
// the services API declares on the root cluster, kept under the same name, so
// a project-scoped reader sees exactly the object a service operator created
// with no separate identity to reconcile.
var serviceAvailabilityMirrorGVK = servicesv1alpha1.GroupVersion.WithKind("ServiceAvailability")

// legacyLocationBindingGVK identifies the former consumer projection. Active
// compute and portal readers use locations.miloapis.com Location now, so the
// reconciler only removes bindings left by older service-catalog releases. A
// control plane where NSO has already removed the CRD is handled by prune as a
// no-op.
var legacyLocationBindingGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha",
	Kind:    "LocationBinding",
}

const (
	// locationProjectionFieldManager identifies writes this reconciler makes.
	locationProjectionFieldManager = "services-operator-location-projection"

	// serviceAvailabilityMirrorFieldManager identifies writes this reconciler
	// makes to mirrored ServiceAvailability objects.
	serviceAvailabilityMirrorFieldManager = "services-operator-serviceavailability-mirror"

	// locationProjectionMaxConcurrentReconciles is how many project clusters this
	// controller projects into at once. One reconcile is one project: it reads
	// that project's entitlements, reads the root cluster's gates, and writes
	// up to a few dozen small objects into the project's control plane — almost
	// entirely API round-trips, so a serial controller spends its time waiting.
	// Activating a service across a fleet enqueues one request per project at
	// once, and at the default of 1 the tail of that queue waits for every
	// project ahead of it (#90).
	//
	// Five is deliberately modest rather than tuned: the write amplification is
	// per-project and the root-cluster reads all hit the same shared cache, so
	// the ceiling that matters is how much concurrent write load a project
	// control plane should see from one operator, not this operator's own
	// throughput. Raising it is not free for the API servers on the other end.
	locationProjectionMaxConcurrentReconciles = 5
)

// LocationProjectionReconciler projects platform Locations into entitled
// projects and mirrors the per-service availability records backing them.
//
// A reconcile is scoped to one project (req.ClusterName) and recomputes desired
// state across every Active ServiceEntitlement there, not just the one that
// triggered it — a Location a project can use is service-agnostic, so no single
// entitlement can own it. Three gates decide whether a service backs a Location:
//
//	gate 1: the Location's class is in the active ServiceConfiguration's
//	        spec.locations.supportedClasses
//	gate 2: the Location's status.conditions[Ready] is True
//	gate 3: a ServiceAvailability for (service, location) reports
//	        status.conditions[Available] = True
//
// The projected Location carries no service availability flag — a flag on a
// record shared by every service
// could only mean that some unnamed service works here, the conflation this
// design removes. It exists while at least one mirrored ServiceAvailability
// reaches it, and otherwise carries only facts about the place: the platform
// Location's own mirrored conditions.
//
// A mirrored ServiceAvailability is a per-project verdict, not a literal copy:
// it exists for (service, location) only when the project holds an Active
// entitlement for the service, the platform reports gate 3 open, and gate 1
// also holds — the service's published configuration supports the location's
// class. The platform record alone only asserts that the service runs at the
// location; a consumer needs both facts to act on it. Presence of the record
// is the signal; it carries no flag of its own to read.
//
// It runs on the multicluster manager so each reconcile is scoped to one
// engaged project cluster (req.ClusterName), where it reads the project's
// ServiceEntitlements and writes projections. ServiceAvailability,
// ServiceConfiguration, and Location all live on the root cluster and are read
// through rootClient.
type LocationProjectionReconciler struct {
	// rootClient reads cluster-scoped ServiceConfiguration and
	// ServiceAvailability objects, and unstructured Location objects, from the
	// root key space. These do not live in any project VCP, so the per-cluster
	// client cannot see them — mirrors ServiceEntitlementReconciler.
	rootClient    client.Client
	rootAPIReader client.Reader
	Manager       mcmanager.Manager
	Scheme        *runtime.Scheme

	projects *projectionProjectIndex
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=locationbindings,verbs=list;delete;deletecollection
// +kubebuilder:rbac:groups=locations.miloapis.com,resources=locations,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=locations.miloapis.com,resources=locations/status,verbs=get;update;patch
// The Milo multicluster provider watches resourcemanager Projects to discover
// which project control planes to engage; without list/watch here its informer
// never syncs and the manager crash-loops on the cache-sync timeout.
// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=projects,verbs=get;list;watch

func (r *LocationProjectionReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	reconcileStarted := time.Now()
	defer func() {
		projectionPhaseDuration.WithLabelValues("total").Observe(time.Since(reconcileStarted).Seconds())
	}()
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("location projection reconcile invoked without a cluster name")
	}
	project, err := getConsumerProject(ctx, r.rootClient, r.rootAPIReader, string(consumerProject))
	if err != nil {
		return ctrl.Result{}, err
	}
	if project == nil || !project.DeletionTimestamp.IsZero() {
		if r.projects != nil {
			r.projects.Remove(string(consumerProject))
		}
		logger.V(1).Info("project is absent or terminating; skipping location projection")
		return ctrl.Result{}, nil
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		if errors.Is(err, multicluster.ErrClusterNotFound) {
			if r.projects != nil {
				r.projects.Remove(string(consumerProject))
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get consumer cluster %q: %w", consumerProject, err)
	}
	consumerClient := consumerCluster.GetClient()

	// The triggering event names one ServiceEntitlement, but a projection's
	// existence now depends on every Active entitlement in the project, not
	// just the one that fired the watch. A deleted, non-Active, or otherwise
	// disqualified entitlement is handled the same way as any other: it is
	// simply absent from active below, and the project-wide recompute
	// naturally stops contributing its Locations and mirrors.
	var entitlementList servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &entitlementList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceEntitlements: %w", err)
	}
	active := make([]*servicesv1alpha1.ServiceEntitlement, 0, len(entitlementList.Items))
	for i := range entitlementList.Items {
		e := &entitlementList.Items[i]
		if e.DeletionTimestamp.IsZero() && e.Status.Phase == servicesv1alpha1.EntitlementPhaseActive {
			active = append(active, e)
		}
	}
	if r.projects != nil {
		services := make([]string, 0, len(active))
		for _, entitlement := range active {
			services = append(services, entitlement.Spec.ServiceRef.Name)
		}
		r.projects.Replace(string(consumerProject), services)
	}
	// Processing order only ever affects which of several equally-closed gates
	// is reported as the aggregate Location's reason; sorting by service name
	// makes that choice deterministic across reconciles instead of depending on
	// list order.
	sort.Slice(active, func(i, j int) bool {
		return active[i].Spec.ServiceRef.Name < active[j].Spec.ServiceRef.Name
	})

	var saList servicesv1alpha1.ServiceAvailabilityList
	if err := r.rootClient.List(ctx, &saList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceAvailabilities: %w", err)
	}
	sort.Slice(saList.Items, func(i, j int) bool { return saList.Items[i].Name < saList.Items[j].Name })

	desiredLocationFields := make(map[string]locationFields)
	desiredMirrors := make(map[string]*servicesv1alpha1.ServiceAvailability)
	locationCache := make(map[string]*unstructured.Unstructured)
	projectionStages := make(map[string]setupProjectionStage)

	for _, entitlement := range active {
		serviceRefName := entitlement.Spec.ServiceRef.Name

		// Gate-source 1: the active (latest Published) ServiceConfiguration for
		// the service supplies spec.locations.supportedClasses. Without one
		// there is nothing to evaluate for this entitlement; it contributes
		// nothing this pass and is picked up when configuration publication
		// produces a root informer event.
		sc, err := r.latestPublishedConfiguration(ctx, serviceRefName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if sc == nil {
			logger.V(1).Info("no published ServiceConfiguration for service yet", "service", serviceRefName)
			projectionStages[serviceRefName] = setupProjectionStage{reason: "ConfigurationUnavailable"}
			continue
		}
		if sc.Spec.Locations == nil || len(sc.Spec.Locations.SupportedClasses) == 0 {
			projectionStages[serviceRefName] = setupProjectionStage{
				complete: true,
				reason:   "NotApplicable",
				revision: fmt.Sprintf("%s/%d", sc.UID, sc.Generation),
			}
			continue
		}
		stage := setupProjectionStage{
			reason:   "NoApplicableLocations",
			revision: fmt.Sprintf("%s/%d", sc.UID, sc.Generation),
		}
		supported := make(map[servicesv1alpha1.LocationClassName]struct{}, len(sc.Spec.Locations.SupportedClasses))
		for _, c := range sc.Spec.Locations.SupportedClasses {
			supported[c] = struct{}{}
		}

		for i := range saList.Items {
			sa := &saList.Items[i]
			if sa.Spec.ServiceRef.Name != serviceRefName {
				continue
			}

			if !apimeta.IsStatusConditionTrue(sa.Status.Conditions, ConditionTypeAvailable) {
				continue
			}

			locName := sa.Spec.LocationRef.Name
			loc, cached := locationCache[locName]
			if !cached {
				// Gate-source 2: load the referenced Location. A transient read
				// failure must requeue without disturbing existing projections —
				// never flip one to unavailable on a blip.
				got, found, err := getLocation(ctx, r.rootClient, locName)
				if err != nil {
					// A location source the control plane does not serve is a
					// misconfiguration, and every location looks absent through
					// it. Returning before any project/prune call below is what
					// stops that from tearing down every projection an entitled
					// project already has.
					return ctrl.Result{}, fmt.Errorf("failed to get Location %q: %w", locName, err)
				}
				if !found {
					// The Location vanished out from under an Available record;
					// treat as a closed gate and let the prune remove any
					// projection.
					locationCache[locName] = nil
					continue
				}
				loc = got
				locationCache[locName] = loc
			}
			if loc == nil {
				continue
			}

			fields := extractLocationFields(loc)
			// The Location projection and its mirrored ServiceAvailability
			// additionally require gate 1: a service that merely runs at a
			// location but does not support its class does not work there for
			// this project, and a consumer needs that fact, not just gate 3.
			if _, classSupported := supported[fields.class]; !classSupported {
				continue
			}
			desiredMirrors[sa.Name] = sa
			stage.complete = true
			stage.reason = "ProjectionConverged"
			if _, ok := desiredLocationFields[locName]; !ok {
				desiredLocationFields[locName] = fields
			}
		}
		projectionStages[serviceRefName] = stage
	}

	for locName, fields := range desiredLocationFields {
		if err := r.projectLocation(ctx, consumerClient, locName, fields); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, consumerClient, []schema.GroupVersionKind{LocationGVK}, keySet(desiredLocationFields)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.prune(ctx, consumerClient, []schema.GroupVersionKind{legacyLocationBindingGVK}, map[string]struct{}{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove legacy LocationBindings: %w", err)
	}

	for _, sa := range desiredMirrors {
		if err := r.mirrorAvailability(ctx, consumerClient, sa); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, consumerClient, []schema.GroupVersionKind{serviceAvailabilityMirrorGVK}, keySet(desiredMirrors)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.aggregateSetup(ctx, string(consumerProject), consumerClient, entitlementList.Items, projectionStages); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled location projections",
		"activeEntitlements", len(active), "locations", len(desiredLocationFields),
		"availabilityMirrors", len(desiredMirrors))
	return ctrl.Result{}, nil
}

type setupProjectionStage struct {
	complete bool
	reason   string
	revision string
}

const (
	setupStageAuthorization = "Authorization"
	setupStageDependencies  = "Dependencies"
	setupStageProvisioning  = "Provisioning"
	setupStageProjection    = "Projection"
	setupStageProviderAck   = "ProviderAcknowledgement"
)

func (r *LocationProjectionReconciler) aggregateSetup(
	ctx context.Context,
	project string,
	consumerClient client.Client,
	entitlements []servicesv1alpha1.ServiceEntitlement,
	projectionStages map[string]setupProjectionStage,
) error {
	var serviceList servicesv1alpha1.ServiceList
	if err := r.rootClient.List(ctx, &serviceList); err != nil {
		return fmt.Errorf("list Services for setup dependency aggregation: %w", err)
	}
	servicesByName := make(map[string]*servicesv1alpha1.Service, len(serviceList.Items))
	for i := range serviceList.Items {
		service := &serviceList.Items[i]
		servicesByName[service.Name] = service
		if service.Spec.ServiceName != "" {
			servicesByName[service.Spec.ServiceName] = service
		}
	}
	entitlementsByService := make(map[string]*servicesv1alpha1.ServiceEntitlement, len(entitlements))
	for i := range entitlements {
		entitlement := &entitlements[i]
		entitlementsByService[entitlement.Spec.ServiceRef.Name] = entitlement
		if entitlement.Status.ServiceName != "" {
			entitlementsByService[entitlement.Status.ServiceName] = entitlement
		}
	}
	// SetupComplete is itself an input to each parent's dependency stage. Walk
	// dependencies before their parents so the in-memory status written below
	// is immediately visible to every ancestor in the same reconcile. Without
	// this ordering, a parent that happened to be returned first by LIST waited
	// for its child's status update to enqueue another controller pass; a
	// compute -> networking -> IPAM chain could therefore require three passes.
	for _, entitlement := range dependencyFirstEntitlements(entitlements, servicesByName, entitlementsByService) {
		before := entitlement.DeepCopy()
		if entitlement.Status.Setup == nil {
			requestedAt := entitlement.CreationTimestamp
			if entitlement.Spec.ProviderActivation != nil && !entitlement.Spec.ProviderActivation.RequestedAt.IsZero() {
				requestedAt = entitlement.Spec.ProviderActivation.RequestedAt
			}
			entitlement.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{
				RequestedAt: requestedAt,
			}
		}
		setup := entitlement.Status.Setup

		ready := apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeReady)
		suspended := apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended)
		authorized := ready && !suspended
		setSetupStage(&setup.Stages, entitlement.Generation, setupStageAuthorization, authorized,
			setupAuthorizationReason(entitlement, ready, suspended), "Access must be authorized and the project active.")
		if authorized && setup.AuthorizedAt == nil {
			if condition := apimeta.FindStatusCondition(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeReady); condition != nil {
				t := condition.LastTransitionTime
				setup.AuthorizedAt = &t
			}
		}

		dependencies, dependencyReason := transitiveDependenciesComplete(
			entitlement, servicesByName, entitlementsByService, map[string]bool{},
		)
		setSetupStage(&setup.Stages, entitlement.Generation, setupStageDependencies, dependencies,
			dependencyReason,
			"Every transitive service dependency must complete enrollment.")

		provisioned := apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
		setSetupStage(&setup.Stages, entitlement.Generation, setupStageProvisioning, provisioned,
			conditionReason(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned, "ProvisioningPending"),
			"Declared service resources must be installed.")

		projection := projectionStages[entitlement.Spec.ServiceRef.Name]
		projectionComplete := projection.complete && entitlement.Status.Phase == servicesv1alpha1.EntitlementPhaseActive
		projectionReason := projection.reason
		if projectionReason == "" {
			projectionReason = "EntitlementNotActive"
		}
		setSetupStage(&setup.Stages, entitlement.Generation, setupStageProjection, projectionComplete,
			projectionReason, "Required service locations and availability records must converge.")
		setup.ConfigurationRevision = projection.revision

		setSetupStage(&setup.Stages, entitlement.Generation, setupStageProviderAck, true,
			"NotApplicable", "This service does not declare a provider acknowledgement stage.")

		complete := authorized && dependencies && provisioned && projectionComplete
		blockingStage := firstIncompleteSetupStage(setup.Stages)
		if complete {
			blockingStage = ""
		}
		apimeta.SetStatusCondition(&entitlement.Status.Conditions, metav1.Condition{
			Type:               servicesv1alpha1.ConditionTypeSetupComplete,
			Status:             conditionStatus(complete),
			Reason:             setupCompleteReason(complete, blockingStage),
			Message:            setupCompleteMessage(complete, blockingStage),
			ObservedGeneration: entitlement.Generation,
		})
		if complete && setup.FirstCompletedAt == nil {
			condition := apimeta.FindStatusCondition(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete)
			t := condition.LastTransitionTime
			observeSetupSuccess(entitlement, t.Time)
			setup.FirstCompletedAt = &t
		}

		if reflect.DeepEqual(before.Status, entitlement.Status) {
			continue
		}
		if err := consumerClient.Status().Patch(ctx, entitlement, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("patch setup status for ServiceEntitlement %q: %w", entitlement.Name, err)
		}
	}
	setupInflight.replaceProject(project, entitlements)
	setupObserverLastSuccess.SetToCurrentTime()
	return nil
}

// dependencyFirstEntitlements returns each entitlement after all dependency
// entitlements reachable from its Service. Cycles are deliberately not an
// error here: transitiveDependenciesComplete detects them and records
// DependencyCycle, while the traversal state merely guarantees termination.
func dependencyFirstEntitlements(
	entitlements []servicesv1alpha1.ServiceEntitlement,
	services map[string]*servicesv1alpha1.Service,
	entitlementsByService map[string]*servicesv1alpha1.ServiceEntitlement,
) []*servicesv1alpha1.ServiceEntitlement {
	const (
		visiting = 1
		visited  = 2
	)
	state := make(map[*servicesv1alpha1.ServiceEntitlement]uint8, len(entitlements))
	ordered := make([]*servicesv1alpha1.ServiceEntitlement, 0, len(entitlements))

	var visit func(*servicesv1alpha1.ServiceEntitlement)
	visit = func(entitlement *servicesv1alpha1.ServiceEntitlement) {
		if entitlement == nil || state[entitlement] == visited {
			return
		}
		if state[entitlement] == visiting {
			return
		}
		state[entitlement] = visiting

		service := services[entitlement.Spec.ServiceRef.Name]
		if service == nil && entitlement.Status.ServiceName != "" {
			service = services[entitlement.Status.ServiceName]
		}
		if service != nil {
			for _, dependency := range service.Spec.Dependencies {
				visit(entitlementsByService[dependency.ServiceRef.Name])
			}
		}

		state[entitlement] = visited
		ordered = append(ordered, entitlement)
	}
	for i := range entitlements {
		visit(&entitlements[i])
	}
	return ordered
}

func transitiveDependenciesComplete(
	entitlement *servicesv1alpha1.ServiceEntitlement,
	services map[string]*servicesv1alpha1.Service,
	entitlements map[string]*servicesv1alpha1.ServiceEntitlement,
	visiting map[string]bool,
) (bool, string) {
	if !apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied) {
		return false, conditionReason(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied, "DependenciesPending")
	}
	service := services[entitlement.Spec.ServiceRef.Name]
	if service == nil && entitlement.Status.ServiceName != "" {
		service = services[entitlement.Status.ServiceName]
	}
	if service == nil || len(service.Spec.Dependencies) == 0 {
		return true, "DependenciesComplete"
	}
	key := service.Name
	if visiting[key] {
		return false, "DependencyCycle"
	}
	visiting[key] = true
	defer delete(visiting, key)
	for _, dependency := range service.Spec.Dependencies {
		child := entitlements[dependency.ServiceRef.Name]
		if child == nil {
			return false, "DependencyEntitlementMissing"
		}
		if childService := services[dependency.ServiceRef.Name]; childService != nil && visiting[childService.Name] {
			return false, "DependencyCycle"
		}
		if complete, reason := transitiveDependenciesComplete(child, services, entitlements, visiting); !complete {
			return false, reason
		}
		if child.Status.Setup == nil || !apimeta.IsStatusConditionTrue(child.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete) {
			return false, "DependencySetupPending"
		}
	}
	return true, "DependenciesComplete"
}

func setSetupStage(conditions *[]metav1.Condition, generation int64, stage string, complete bool, reason, message string) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: stage, Status: conditionStatus(complete), Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func conditionReason(conditions []metav1.Condition, conditionType, fallback string) string {
	if condition := apimeta.FindStatusCondition(conditions, conditionType); condition != nil && condition.Reason != "" {
		return condition.Reason
	}
	return fallback
}

func setupAuthorizationReason(entitlement *servicesv1alpha1.ServiceEntitlement, ready, suspended bool) string {
	if suspended {
		return "ProjectSuspended"
	}
	if ready {
		return "Authorized"
	}
	return conditionReason(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeReady, "AuthorizationPending")
}

func firstIncompleteSetupStage(stages []metav1.Condition) string {
	for _, stage := range []string{setupStageAuthorization, setupStageDependencies, setupStageProvisioning, setupStageProjection, setupStageProviderAck} {
		if !apimeta.IsStatusConditionTrue(stages, stage) {
			return stage
		}
	}
	return ""
}

func setupCompleteReason(complete bool, blocker string) string {
	if complete {
		return "AllStagesComplete"
	}
	return blocker + "Pending"
}

func setupCompleteMessage(complete bool, blocker string) string {
	if complete {
		return "Service catalog setup is complete."
	}
	return fmt.Sprintf("Service catalog setup is waiting on the %s stage.", blocker)
}

// keySet extracts the key set of a map as a set, for prune's keep argument.
func keySet[V any](m map[string]V) map[string]struct{} {
	keep := make(map[string]struct{}, len(m))
	for k := range m {
		keep[k] = struct{}{}
	}
	return keep
}

// latestPublishedConfiguration returns the most recently created Published
// ServiceConfiguration for the named service, or nil if none exists. Selecting
// the latest published document (rather than a pinned reference) is the agreed
// configuration-selection behavior for the location gates.
func (r *LocationProjectionReconciler) latestPublishedConfiguration(
	ctx context.Context,
	serviceRefName string,
) (*servicesv1alpha1.ServiceConfiguration, error) {
	var list servicesv1alpha1.ServiceConfigurationList
	if err := r.rootClient.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("failed to list ServiceConfigurations: %w", err)
	}
	var latest *servicesv1alpha1.ServiceConfiguration
	for i := range list.Items {
		sc := &list.Items[i]
		if sc.Spec.ServiceRef.Name != serviceRefName {
			continue
		}
		if sc.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}
		if latest == nil || moreRecent(sc, latest) {
			latest = sc
		}
	}
	return latest, nil
}

// moreRecent reports whether a should win over b as the active configuration:
// later creation timestamp, breaking ties on the higher name for determinism.
func moreRecent(a, b *servicesv1alpha1.ServiceConfiguration) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name > b.Name
	}
	return a.CreationTimestamp.After(b.CreationTimestamp.Time)
}

// locationFields are the consumer-visible bits the reconciler copies from a
// platform Location onto its project-scoped projection.
type locationFields struct {
	class servicesv1alpha1.LocationClassName
	// topology mirrors the referenced Location's spec.topology verbatim. Both
	// projections carry the well-known keys (e.g.
	// topology.datum.net/city-code, topology.datum.net/region) that downstream
	// consumers like the compute workload webhook read to resolve a location's
	// valid city codes.
	topology map[string]string
	// coordinates mirrors spec.coordinates where the source Location carries
	// it.
	coordinates map[string]any
	// conditions are the platform Location's own status conditions, mirrored
	// onto the projection so a consumer reading only their own control plane
	// sees why a location is in the state it is in. Available is excluded
	// because service availability belongs on ServiceAvailability.
	conditions []metav1.Condition
}

// extractLocationFields pulls the projection fields out of an unstructured
// Location. Every field is best-effort; projectionSpec enforces the fields the
// consumer-facing Location schema requires.
func extractLocationFields(loc *unstructured.Unstructured) locationFields {
	var f locationFields
	f.class = locationClass(loc)
	// City, region, and other location attributes are carried in spec.topology under
	// datum.net topology keys. Mirror the whole map verbatim.
	f.topology, _, _ = unstructured.NestedStringMap(loc.Object, "spec", "topology")
	f.coordinates, _, _ = unstructured.NestedMap(loc.Object, "spec", "coordinates")
	f.conditions = objectConditions(loc)
	return f
}

// projectLocation writes the locations.miloapis.com Location projection for
// one location. It carries the platform Location's own mirrored conditions
// and nothing else — no Available condition, aggregate or otherwise: a flag on
// a record shared by every service in the project could only say that some
// unnamed service works here, which is what the mirrored ServiceAvailability
// exists to say instead.
func (r *LocationProjectionReconciler) projectLocation(
	ctx context.Context,
	consumerClient client.Client,
	locName string,
	fields locationFields,
) error {
	spec, ok := projectionSpec(fields)
	if !ok {
		return nil
	}
	if err := r.upsertProjection(ctx, consumerClient, spec, locName, fields.conditions); err != nil {
		return fmt.Errorf("failed to upsert %s %q: %w", LocationGVK.Kind, locName, err)
	}
	return nil
}

// projectionSpec builds the spec for one projection kind, reporting false when
// the source location cannot satisfy that kind's schema.
func projectionSpec(fields locationFields) (map[string]any, bool) {
	topology := make(map[string]any, len(fields.topology))
	for k, v := range fields.topology {
		topology[k] = v
	}

	if fields.class == "" || len(topology) == 0 {
		return nil, false
	}
	spec := map[string]any{
		"locationClassRef": map[string]any{"name": string(fields.class)},
		"topology":         topology,
	}
	if len(fields.coordinates) > 0 {
		spec["coordinates"] = fields.coordinates
	}
	return spec, true
}

// upsertProjection creates or updates one projected object for one location and
// reconciles its status. Metadata/spec are written via CreateOrUpdate; status is
// written separately (and only when it changes) so an already-settled projection
// is a no-op.
//
// The projection carries the platform Location's own conditions.
func (r *LocationProjectionReconciler) upsertProjection(
	ctx context.Context,
	consumerClient client.Client,
	spec map[string]any,
	locName string,
	mirrored []metav1.Condition,
) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(LocationGVK)
	u.SetName(locName)

	if _, err := controllerutil.CreateOrUpdate(ctx, consumerClient, u, func() error {
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelManagedBy] = labelManagedByValue
		delete(labels, "networking.datumapis.com/location")
		delete(labels, "networking.datumapis.com/class")
		delete(labels, "services.miloapis.com/service-name")
		u.SetLabels(labels)

		// A projection's existence now depends on whether any active
		// entitlement in the project still needs it, not on which one happened
		// to create it, so it is no longer Kubernetes-owned by a
		// ServiceEntitlement; garbage collection is driven by prune's
		// desired-state computation instead. An owner reference left behind
		// from before this change would cascade-delete a projection other
		// entitlements still need, so it is explicitly cleared here rather
		// than left for CreateOrUpdate to ignore.
		u.SetOwnerReferences(nil)
		return unstructured.SetNestedMap(u.Object, spec, "spec")
	}); err != nil {
		if apimeta.IsNoMatchError(err) {
			// The kind is not installed in this consumer control plane.
			return nil
		}
		return err
	}

	return r.setProjectionStatus(ctx, consumerClient, u, mirrored)
}

// setProjectionStatus reconciles a projection's conditions: the platform
// Location's own conditions mirrored verbatim, plus this reconciler's Available
// verdict where the kind carries one. It writes the status subresource only
// when the set actually changes, so a settled projection costs one comparison
// and no write.
//
// The mirrored conditions are the whole reason a consumer can act on a
// locations.miloapis.com projection at all: Ready and its reason say which
// platform-side fact is unmet, and they are only ever written on the platform
// copy the consumer cannot see. Any Available condition already present is
// dropped from that mirror because service availability is represented by the
// separate ServiceAvailability projection.
//
// observedGeneration is deliberately not carried across: on a mirrored condition
// it refers to the platform Location's generation, which means nothing against
// the projection's own.
func (r *LocationProjectionReconciler) setProjectionStatus(
	ctx context.Context,
	consumerClient client.Client,
	u *unstructured.Unstructured,
	mirrored []metav1.Condition,
) error {
	before := objectConditions(u)

	after := make([]metav1.Condition, 0, len(mirrored))
	for _, c := range mirrored {
		if c.Type == ConditionTypeAvailable {
			continue
		}
		after = append(after, c)
	}

	if conditionSetsEqual(before, after) {
		return nil
	}
	if err := setObjectConditions(u, after); err != nil {
		return err
	}
	if err := consumerClient.Status().Update(ctx, u, client.FieldOwner(locationProjectionFieldManager)); err != nil {
		return fmt.Errorf("failed to update %s status: %w", u.GetKind(), err)
	}
	return nil
}

// conditionSetsEqual compares two condition sets by type, ignoring order and
// lastTransitionTime. A mirrored condition whose only change is its transition
// time is not worth a write.
func conditionSetsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		other := apimeta.FindStatusCondition(b, a[i].Type)
		if other == nil ||
			other.Status != a[i].Status ||
			other.Reason != a[i].Reason ||
			other.Message != a[i].Message {
			return false
		}
	}
	return true
}

// mirrorAvailability upserts a project-scoped copy of a platform
// ServiceAvailability, carrying its spec and status verbatim. The copy is only
// ever made for a (service, location) pair that has already cleared gate 1
// (class support) in the caller, on top of the platform's own gate 3 verdict
// carried in its status — the mirror's existence is a stronger claim than the
// platform record alone makes, even though the condition payload is copied,
// not recomputed.
func (r *LocationProjectionReconciler) mirrorAvailability(
	ctx context.Context,
	consumerClient client.Client,
	source *servicesv1alpha1.ServiceAvailability,
) error {
	mirror := &servicesv1alpha1.ServiceAvailability{}
	mirror.Name = source.Name

	if _, err := controllerutil.CreateOrUpdate(ctx, consumerClient, mirror, func() error {
		labels := mirror.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelManagedBy] = labelManagedByValue
		mirror.SetLabels(labels)
		mirror.Spec = source.Spec
		return nil
	}); err != nil {
		if apimeta.IsNoMatchError(err) {
			// ServiceAvailability is not installed in this consumer control
			// plane; nothing to mirror into.
			return nil
		}
		return fmt.Errorf("failed to upsert mirrored ServiceAvailability %q: %w", source.Name, err)
	}

	if conditionsEqual(mirror.Status.Conditions, source.Status.Conditions, ConditionTypeAvailable) &&
		mirror.Status.ObservedGeneration == source.Status.ObservedGeneration {
		return nil
	}
	mirror.Status = source.Status
	if err := consumerClient.Status().Update(ctx, mirror, client.FieldOwner(serviceAvailabilityMirrorFieldManager)); err != nil {
		return fmt.Errorf("failed to update mirrored ServiceAvailability %q status: %w", source.Name, err)
	}
	return nil
}

// objectConditions parses status.conditions off an unstructured object into
// typed conditions. LastTransitionTime is preserved so apimeta.SetStatusCondition
// can decide whether a write is needed.
func objectConditions(u *unstructured.Unstructured) []metav1.Condition {
	raw, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	out := make([]metav1.Condition, 0, len(raw))
	for _, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		cond := metav1.Condition{}
		cond.Type, _, _ = unstructured.NestedString(m, "type")
		s, _, _ := unstructured.NestedString(m, "status")
		cond.Status = metav1.ConditionStatus(s)
		cond.Reason, _, _ = unstructured.NestedString(m, "reason")
		cond.Message, _, _ = unstructured.NestedString(m, "message")
		cond.ObservedGeneration, _, _ = unstructured.NestedInt64(m, "observedGeneration")
		if lt, ok, _ := unstructured.NestedString(m, "lastTransitionTime"); ok {
			if t, err := time.Parse(time.RFC3339, lt); err == nil {
				cond.LastTransitionTime = metav1.NewTime(t)
			}
		}
		out = append(out, cond)
	}
	return out
}

// setObjectConditions writes typed conditions back onto an unstructured
// object's status.conditions.
func setObjectConditions(u *unstructured.Unstructured, conds []metav1.Condition) error {
	arr := make([]any, 0, len(conds))
	for _, c := range conds {
		entry := map[string]any{
			"type":               c.Type,
			"status":             string(c.Status),
			"reason":             c.Reason,
			"message":            c.Message,
			"lastTransitionTime": c.LastTransitionTime.UTC().Format(time.RFC3339),
		}
		arr = append(arr, entry)
	}
	return unstructured.SetNestedSlice(u.Object, arr, "status", "conditions")
}

// prune deletes managed objects of the given kinds that are not in the keep
// set. Garbage collection is driven entirely by this desired-state comparison
// rather than a Kubernetes owner-reference cascade, the same mark-and-sweep
// pattern QuotaFanOut and BillingFanOut already use: it lists by the shared
// managed-by label and deletes whatever the caller no longer wants, with no
// notion of which entitlement a projection came from.
func (r *LocationProjectionReconciler) prune(
	ctx context.Context,
	consumerClient client.Client,
	gvks []schema.GroupVersionKind,
	keep map[string]struct{},
) error {
	for _, gvk := range gvks {
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := consumerClient.List(ctx, &list,
			client.MatchingLabelsSelector{Selector: managedByFanoutSelector},
		); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				// The kind is not installed in this project cluster; nothing
				// to prune.
				continue
			}
			return fmt.Errorf("failed to list %s: %w", gvk.Kind, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if _, ok := keep[item.GetName()]; ok {
				continue
			}
			if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete stale %s %q: %w", gvk.Kind, item.GetName(), err)
			}
		}
	}
	return nil
}

// locationProjectionRequestName collapses every entitlement's events in a
// project onto one workqueue key, so N changes trigger one reconcile.
const locationProjectionRequestName = "project"

// TypedEnqueueRequestsFromMapFunc fills in the cluster name afterward.
func mapServiceEntitlementToProjectRequest(_ context.Context, _ client.Object) []mcreconcile.Request {
	return []mcreconcile.Request{{
		Request: reconcile.Request{
			NamespacedName: types.NamespacedName{Name: locationProjectionRequestName},
		},
	}}
}

// projectionProjectIndex is rebuilt from the complete entitlement set each
// time a project is reconciled. Root-source events use the snapshot to target
// projects without periodically walking the fleet. Startup entitlement LIST
// events repopulate it after a restart, so correctness does not depend on old
// watch events being replayed.
type projectionProjectIndex struct {
	mu       sync.RWMutex
	services map[string]map[string]struct{}
}

func newProjectionProjectIndex() *projectionProjectIndex {
	return &projectionProjectIndex{services: make(map[string]map[string]struct{})}
}

func (i *projectionProjectIndex) Replace(project string, serviceNames []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for service, projects := range i.services {
		delete(projects, project)
		if len(projects) == 0 {
			delete(i.services, service)
		}
	}
	for _, service := range serviceNames {
		projects := i.services[service]
		if projects == nil {
			projects = make(map[string]struct{})
			i.services[service] = projects
		}
		projects[project] = struct{}{}
	}
}

func (i *projectionProjectIndex) Remove(project string) {
	i.Replace(project, nil)
}

func (i *projectionProjectIndex) ProjectsForService(service string) []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	projects := make([]string, 0, len(i.services[service]))
	for project := range i.services[service] {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

func (i *projectionProjectIndex) Projects() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	set := make(map[string]struct{})
	for _, projects := range i.services {
		for project := range projects {
			set[project] = struct{}{}
		}
	}
	projects := make([]string, 0, len(set))
	for project := range set {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

// projectionRootDispatcher attaches handlers directly to the shared root
// informers. It runs on every replica because cache observation is also needed
// on the projection-lease holder when the primary manager lease is held by a
// different replica. Only the elected projection controller consumes and
// mutates from these events.
type projectionRootDispatcher struct {
	rootMgr manager.Manager
	gvk     schema.GroupVersionKind
	index   *projectionProjectIndex
	events  chan<- event.TypedGenericEvent[mcreconcile.Request]
}

func (d *projectionRootDispatcher) NeedLeaderElection() bool { return false }

func (d *projectionRootDispatcher) Start(ctx context.Context) error {
	objects := []client.Object{
		&servicesv1alpha1.ServiceAvailability{},
		&servicesv1alpha1.ServiceConfiguration{},
	}
	location := &unstructured.Unstructured{}
	location.SetGroupVersionKind(d.gvk)
	objects = append(objects, location)

	for _, object := range objects {
		informer, err := d.rootMgr.GetCache().GetInformer(ctx, object)
		if err != nil {
			return fmt.Errorf("get required projection source informer for %s: %w", object.GetObjectKind().GroupVersionKind(), err)
		}
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { d.dispatchObjects(ctx, obj) },
			UpdateFunc: func(oldObj, newObj any) { d.dispatchObjects(ctx, oldObj, newObj) },
			DeleteFunc: func(obj any) { d.dispatchObjects(ctx, obj) },
		}); err != nil {
			return fmt.Errorf("register projection source handler for %s: %w", object.GetObjectKind().GroupVersionKind(), err)
		}
	}
	if !d.rootMgr.GetCache().WaitForCacheSync(ctx) {
		return fmt.Errorf("required projection source caches did not synchronize")
	}
	setupObserverHealthy.Set(1)
	defer setupObserverHealthy.Set(0)
	<-ctx.Done()
	return nil
}

func (d *projectionRootDispatcher) dispatchObjects(ctx context.Context, objects ...any) {
	projects := make(map[string]struct{})
	for _, object := range objects {
		if tombstone, ok := object.(toolscache.DeletedFinalStateUnknown); ok {
			object = tombstone.Obj
		}
		var selected []string
		switch object := object.(type) {
		case *servicesv1alpha1.ServiceAvailability:
			selected = d.index.ProjectsForService(object.Spec.ServiceRef.Name)
		case *servicesv1alpha1.ServiceConfiguration:
			selected = d.index.ProjectsForService(object.Spec.ServiceRef.Name)
		default:
			// Location changes can affect every service, and an unfamiliar event
			// must favor a complete resync over a missed invalidation.
			selected = d.index.Projects()
		}
		for _, project := range selected {
			projects[project] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(projects))
	for project := range projects {
		ordered = append(ordered, project)
	}
	sort.Strings(ordered)
	for _, project := range ordered {
		req := mcreconcile.Request{
			Request:     reconcile.Request{NamespacedName: types.NamespacedName{Name: locationProjectionRequestName}},
			ClusterName: multicluster.ClusterName(project),
		}
		select {
		case d.events <- event.TypedGenericEvent[mcreconcile.Request]{Object: req}:
		case <-ctx.Done():
			return
		}
	}
}

// SetupWithManager registers the reconciler on the multicluster manager.
//
// The primary watch is ServiceEntitlement, scoped to engaged project clusters
// (WithEngageWithProviderClusters), mirroring ServiceEntitlementReconciler —
// via Watches rather than For, so events collapse onto one project-keyed
// request (see locationProjectionRequestName).
//
// Setup-only status updates are intentionally not filtered from this watch.
// Dependency-first aggregation normally makes their follow-up pass a no-op,
// but the event is the recovery trigger if the process stops after patching a
// dependency and before patching its parent.
//
// Root ServiceAvailability, ServiceConfiguration, and canonical Location
// informer events are translated by projectionRootDispatcher into the same
// project key. Managed projection objects are watched in each project so
// deletion and drift repair do not depend on a periodic sweep.
//
// Reconciles run concurrently across projects. Two passes over the same
// project are serialized by controller-runtime's per-key locking, since every
// entitlement in a project maps to the same key, so the concurrency here only
// puts different projects in flight at once. See
// locationProjectionMaxConcurrentReconciles.
func (r *LocationProjectionReconciler) SetupWithManager(mgr mcmanager.Manager, rootMgr manager.Manager) error {
	r.rootClient = rootMgr.GetClient()
	r.rootAPIReader = rootMgr.GetAPIReader()
	r.Manager = mgr
	r.projects = newProjectionProjectIndex()
	rootEvents := make(chan event.TypedGenericEvent[mcreconcile.Request], 4096)
	if err := rootMgr.Add(&projectionRootDispatcher{
		rootMgr: rootMgr,
		gvk:     LocationGVK,
		index:   r.projects,
		events:  rootEvents,
	}); err != nil {
		return fmt.Errorf("add projection root dispatcher: %w", err)
	}
	rootSource := source.TypedChannel[mcreconcile.Request, mcreconcile.Request](
		rootEvents,
		handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, req mcreconcile.Request) []mcreconcile.Request {
			return []mcreconcile.Request{req}
		}),
		source.WithBufferSize[mcreconcile.Request, mcreconcile.Request](4096),
	)
	builder := mcbuilder.ControllerManagedBy(mgr).
		Named("location-projection").
		WatchesRawSource(rootSource).
		Watches(
			&servicesv1alpha1.ServiceEntitlement{},
			mchandler.TypedEnqueueRequestsFromMapFunc[client.Object, mcreconcile.Request](mapServiceEntitlementToProjectRequest),
			mcbuilder.WithEngageWithProviderClusters(true),
		)
	projection := &unstructured.Unstructured{}
	projection.SetGroupVersionKind(LocationGVK)
	builder = builder.Watches(
		projection,
		mchandler.TypedEnqueueRequestsFromMapFunc[client.Object, mcreconcile.Request](mapServiceEntitlementToProjectRequest),
		mcbuilder.WithEngageWithProviderClusters(true),
	)
	builder = builder.Watches(
		&servicesv1alpha1.ServiceAvailability{},
		mchandler.TypedEnqueueRequestsFromMapFunc[client.Object, mcreconcile.Request](mapServiceEntitlementToProjectRequest),
		mcbuilder.WithEngageWithProviderClusters(true),
	)
	return builder.WithOptions(controller.TypedOptions[mcreconcile.Request]{
		MaxConcurrentReconciles: locationProjectionMaxConcurrentReconciles,
	}).
		Complete(r)
}
