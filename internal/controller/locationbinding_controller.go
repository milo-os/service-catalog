// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// locationBindingGVK is the projection this reconciler is moving off.
// LocationBinding is owned by the network-services operator
// (networking.datumapis.com) and is still read by that operator's
// NetworkPresence controller, so it keeps being written alongside the Location
// projection until those consumers move. See projectionGVKs.
var locationBindingGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha",
	Kind:    "LocationBinding",
}

// projectedLocationGVK is the consumer-facing object going forward: the same
// Location kind the locations service declares, projected into a project that
// may use it. What is projected is deliberately independent of which group
// locations are READ from (config.LocationSource) — a control plane can serve
// consumers the new object while still sourcing locations from the old group,
// and the two move on their own schedules.
var projectedLocationGVK = schema.GroupVersionKind{
	Group:   "locations.miloapis.com",
	Version: "v1alpha1",
	Kind:    "Location",
}

// projectionGVKs are the two kinds a reconcile pass owns in a consumer control
// plane. Each is written and pruned against its own desired set — a
// LocationBinding's existence and Available verdict no longer coincide with
// the Location projection's, since the latter also requires the class gate. A
// kind whose CRD is not installed in a given control plane is skipped rather
// than treated as a failure, so a control plane needs only the kinds its
// consumers actually read.
var projectionGVKs = []schema.GroupVersionKind{projectedLocationGVK, locationBindingGVK}

// serviceAvailabilityMirrorGVK identifies the mirrored copy of a platform
// ServiceAvailability written into an entitled project. It is the same Kind
// the services API declares on the root cluster, kept under the same name, so
// a project-scoped reader sees exactly the object a service operator created
// with no separate identity to reconcile.
var serviceAvailabilityMirrorGVK = servicesv1alpha1.GroupVersion.WithKind("ServiceAvailability")

const (
	// locationBindingResyncInterval bounds how long a gate change on the root
	// cluster (ServiceAvailability flips, Location readiness, a new published
	// ServiceConfiguration) takes to propagate into project bindings. The
	// primary trigger is the ServiceEntitlement watch; multicluster-runtime
	// does not give us a clean way to enqueue project-scoped reconciles from
	// root-cluster object events, so a periodic resync covers the cross-cluster
	// gates instead. See SetupWithManager.
	locationBindingResyncInterval = 5 * time.Minute

	// LocationBinding metadata labels. labelLocation and labelClass mirror the
	// referenced Location for label-selector discovery without a platform
	// lookup.
	labelLocation = "networking.datumapis.com/location"
	labelClass    = "networking.datumapis.com/class"

	// labelServiceName previously recorded which service projected a binding.
	// It is retained only as a constant for tests exercising pre-migration
	// state: once a Location projection can represent every service an active
	// entitlement in the project uses, a single service name on it is no
	// longer meaningful, so upsertProjection stops writing it.
	labelServiceName = "services.miloapis.com/service-name"

	// locationBindingFieldManager identifies writes this reconciler makes to
	// LocationBinding and Location projection objects.
	locationBindingFieldManager = "services-operator-locationbinding"

	// serviceAvailabilityMirrorFieldManager identifies writes this reconciler
	// makes to mirrored ServiceAvailability objects.
	serviceAvailabilityMirrorFieldManager = "services-operator-serviceavailability-mirror"

	// reasonAllGatesOpen is the Available=True reason: class supported,
	// Location Ready, and ServiceAvailability Available.
	reasonAllGatesOpen = "AllGatesOpen"

	// reasonLocationClassNotSupported is the Available=False reason when the
	// Location's class is not in the active ServiceConfiguration's
	// supportedClasses (gate 1 closed).
	reasonLocationClassNotSupported = "LocationClassNotSupported"
	// reasonLocationNotReady ("LocationNotReady", gate 2) is shared with the
	// ServiceAvailability reconciler; it is declared there.

	// locationBindingMaxConcurrentReconciles is how many project clusters this
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
	locationBindingMaxConcurrentReconciles = 5
)

// LocationBindingReconciler projects platform Locations into entitled projects
// as consumer-facing, cluster-scoped objects, and mirrors the per-project
// availability records backing them. It writes two Location projection kinds
// while the platform moves onto the locations service: a locations.miloapis.com
// Location, which is what consumers read going forward, and the LocationBinding
// the network-services operator still reads.
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
// The two projection kinds no longer share one verdict. LocationBinding keeps
// its existing contract unchanged: it exists once some active entitlement's
// gate 3 is open, and carries the aggregate of every entitled service's
// verdict at that Location as its own Available condition, true as soon as any
// one of them has every gate open. The locations.miloapis.com Location carries
// no availability flag at all — a flag on a record shared by every service
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
type LocationBindingReconciler struct {
	// rootClient reads cluster-scoped ServiceConfiguration and
	// ServiceAvailability objects, and unstructured Location objects, from the
	// root key space. These do not live in any project VCP, so the per-cluster
	// client cannot see them — mirrors ServiceEntitlementReconciler.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme

	// LocationGVK is the configured location source. Only this group is read.
	// It is unrelated to projectedLocationGVK, which is what gets written.
	LocationGVK schema.GroupVersionKind
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=locations,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=locationbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=locations.miloapis.com,resources=locations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=locations.miloapis.com,resources=locations/status,verbs=get;update;patch
// The Milo multicluster provider watches resourcemanager Projects to discover
// which project control planes to engage; without list/watch here its informer
// never syncs and the manager crash-loops on the cache-sync timeout.
// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=projects,verbs=get;list;watch

func (r *LocationBindingReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("LocationBinding reconcile invoked without a cluster name")
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
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

	desiredBindings := make(map[string]*bindingVerdict)
	desiredLocationFields := make(map[string]locationFields)
	desiredMirrors := make(map[string]*servicesv1alpha1.ServiceAvailability)
	locationCache := make(map[string]*unstructured.Unstructured)

	for _, entitlement := range active {
		serviceRefName := entitlement.Spec.ServiceRef.Name

		// Gate-source 1: the active (latest Published) ServiceConfiguration for
		// the service supplies spec.locations.supportedClasses. Without one
		// there is nothing to evaluate for this entitlement; it contributes
		// nothing this pass and is picked up again on the periodic resync.
		sc, err := r.latestPublishedConfiguration(ctx, serviceRefName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if sc == nil {
			logger.V(1).Info("no published ServiceConfiguration for service yet", "service", serviceRefName)
			continue
		}
		if sc.Spec.Locations == nil || len(sc.Spec.Locations.SupportedClasses) == 0 {
			continue
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

			// LocationBinding's contract is unchanged: it exists once gate 3
			// (this record's own Available) is open, regardless of gates 1 and
			// 2, which only shape its Available verdict below.
			if !apimeta.IsStatusConditionTrue(sa.Status.Conditions, ConditionTypeAvailable) {
				continue
			}

			locName := sa.Spec.LocationRef.Name
			loc, cached := locationCache[locName]
			if !cached {
				// Gate-source 2: load the referenced Location. A transient read
				// failure must requeue without disturbing existing projections —
				// never flip one to unavailable on a blip.
				got, found, err := getLocation(ctx, r.rootClient, r.LocationGVK, locName)
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
			available, reason, message := evaluateGates(fields.class, supported, locationReady(loc), locName)

			v, ok := desiredBindings[locName]
			if !ok {
				desiredBindings[locName] = &bindingVerdict{
					fields: fields, available: available, reason: reason, message: message,
				}
			} else if available && !v.available {
				// The binding's Available condition is the aggregate across
				// every entitled service reaching it: True as soon as one of
				// them has every gate open. A later entitlement whose gates
				// are still closed must not downgrade a verdict an earlier
				// one already opened.
				v.available, v.reason, v.message = available, reason, message
			}

			// The Location projection and its mirrored ServiceAvailability
			// additionally require gate 1: a service that merely runs at a
			// location but does not support its class does not work there for
			// this project, and a consumer needs that fact, not just gate 3.
			if _, classSupported := supported[fields.class]; !classSupported {
				continue
			}
			desiredMirrors[sa.Name] = sa
			if _, ok := desiredLocationFields[locName]; !ok {
				desiredLocationFields[locName] = fields
			}
		}
	}

	for locName, v := range desiredBindings {
		if err := r.projectBinding(ctx, consumerClient, locName, v.fields, v.available, v.reason, v.message); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, consumerClient, []schema.GroupVersionKind{locationBindingGVK}, keySet(desiredBindings)); err != nil {
		return ctrl.Result{}, err
	}

	for locName, fields := range desiredLocationFields {
		if err := r.projectLocation(ctx, consumerClient, locName, fields); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, consumerClient, []schema.GroupVersionKind{projectedLocationGVK}, keySet(desiredLocationFields)); err != nil {
		return ctrl.Result{}, err
	}

	for _, sa := range desiredMirrors {
		if err := r.mirrorAvailability(ctx, consumerClient, sa); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, consumerClient, []schema.GroupVersionKind{serviceAvailabilityMirrorGVK}, keySet(desiredMirrors)); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled location projections",
		"activeEntitlements", len(active), "bindings", len(desiredBindings),
		"locations", len(desiredLocationFields), "availabilityMirrors", len(desiredMirrors))
	return ctrl.Result{RequeueAfter: locationBindingResyncInterval}, nil
}

// bindingVerdict accumulates one LocationBinding's aggregate projection state
// across every active entitlement contributing to it. fields come straight off
// the platform Location and are identical regardless of which entitlement led
// the reconciler there, since they all name the same Location.
type bindingVerdict struct {
	fields    locationFields
	available bool
	reason    string
	message   string
}

// keySet extracts the key set of a map as a set, for prune's keep argument.
func keySet[V any](m map[string]V) map[string]struct{} {
	keep := make(map[string]struct{}, len(m))
	for k := range m {
		keep[k] = struct{}{}
	}
	return keep
}

// evaluateGates resolves gates 1 and 2 for a location whose gate 3
// (ServiceAvailability Available) is already open, returning the Available
// condition status/reason/message for its binding.
func evaluateGates(
	class servicesv1alpha1.LocationClassName,
	supported map[servicesv1alpha1.LocationClassName]struct{},
	ready bool,
	locName string,
) (bool, string, string) {
	if _, ok := supported[class]; !ok {
		return false, reasonLocationClassNotSupported,
			fmt.Sprintf("This service isn't offered for %q locations.", class)
	}
	if !ready {
		return false, reasonLocationNotReady,
			fmt.Sprintf("Service isn't available here yet because the %q location isn't ready.", locName)
	}
	return true, reasonAllGatesOpen, "Service is available at this location."
}

// latestPublishedConfiguration returns the most recently created Published
// ServiceConfiguration for the named service, or nil if none exists. Selecting
// the latest published document (rather than a pinned reference) is the agreed
// configuration-selection behavior for the location gates.
func (r *LocationBindingReconciler) latestPublishedConfiguration(
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
// platform Location onto its projected binding.
type locationFields struct {
	class       servicesv1alpha1.LocationClassName
	displayName string
	// topology mirrors the referenced Location's spec.topology verbatim. Both
	// projections expect spec.topology to carry the well-known keys (e.g.
	// topology.datum.net/city-code, topology.datum.net/region) that downstream
	// consumers like the compute workload webhook read to resolve a location's
	// valid city codes.
	topology map[string]string
	// coordinates mirrors spec.coordinates where the source Location carries
	// it. Only locations.miloapis.com defines the field.
	coordinates map[string]any
	// conditions are the platform Location's own status conditions, mirrored
	// onto the projection so a consumer reading only their own control plane
	// sees why a location is in the state it is in. Available is excluded on
	// the way out: that condition is this reconciler's combined verdict, not
	// the platform's.
	conditions []metav1.Condition
}

// extractLocationFields pulls the projection fields out of an unstructured
// Location. Every field is best-effort: a Location missing optional metadata
// still yields a binding, just with the corresponding fields omitted.
func extractLocationFields(loc *unstructured.Unstructured) locationFields {
	var f locationFields
	f.class = locationClass(loc)
	// displayName exists only on the network-services Location; a location read
	// from the locations service simply has none to carry.
	f.displayName, _, _ = unstructured.NestedString(loc.Object, "spec", "displayName")
	// City, region, and other location attributes are not first-class spec
	// fields on either Location; they are carried in spec.topology under
	// datum.net topology keys. Mirror the whole map verbatim.
	f.topology, _, _ = unstructured.NestedStringMap(loc.Object, "spec", "topology")
	f.coordinates, _, _ = unstructured.NestedMap(loc.Object, "spec", "coordinates")
	f.conditions = objectConditions(loc)
	return f
}

// projectBinding writes the LocationBinding projection for one location,
// carrying this reconciler's aggregate Available verdict. A kind whose CRD is
// absent from the consumer control plane is skipped, so a project that does
// not read LocationBinding does not have to install it.
func (r *LocationBindingReconciler) projectBinding(
	ctx context.Context,
	consumerClient client.Client,
	locName string,
	fields locationFields,
	available bool,
	reason, message string,
) error {
	spec, ok := projectionSpec(locationBindingGVK, locName, fields)
	if !ok {
		return nil
	}
	verdict := &availabilityVerdict{available: available, reason: reason, message: message}
	if err := r.upsertProjection(ctx, consumerClient, locationBindingGVK, spec, locName, fields, nil, verdict); err != nil {
		return fmt.Errorf("failed to upsert %s %q: %w", locationBindingGVK.Kind, locName, err)
	}
	return nil
}

// projectLocation writes the locations.miloapis.com Location projection for
// one location. It carries the platform Location's own mirrored conditions
// and nothing else — no Available condition, aggregate or otherwise: a flag on
// a record shared by every service in the project could only say that some
// unnamed service works here, which is what the mirrored ServiceAvailability
// exists to say instead.
func (r *LocationBindingReconciler) projectLocation(
	ctx context.Context,
	consumerClient client.Client,
	locName string,
	fields locationFields,
) error {
	spec, ok := projectionSpec(projectedLocationGVK, locName, fields)
	if !ok {
		return nil
	}
	if err := r.upsertProjection(ctx, consumerClient, projectedLocationGVK, spec, locName, fields, fields.conditions, nil); err != nil {
		return fmt.Errorf("failed to upsert %s %q: %w", projectedLocationGVK.Kind, locName, err)
	}
	return nil
}

// projectionSpec builds the spec for one projection kind, reporting false when
// the source location cannot satisfy that kind's schema.
func projectionSpec(gvk schema.GroupVersionKind, locName string, fields locationFields) (map[string]any, bool) {
	topology := make(map[string]any, len(fields.topology))
	for k, v := range fields.topology {
		topology[k] = v
	}

	if gvk == projectedLocationGVK {
		// locations.miloapis.com requires both a class name and a non-empty
		// topology. A source location carrying neither can still be projected
		// as the legacy binding, so skip this kind rather than failing.
		if fields.class == "" || len(topology) == 0 {
			return nil, false
		}
		// The class is named without a project qualifier: the consumer control
		// plane does not hold the platform's LocationClass, and no locations
		// controller runs there to resolve it. This reconciler owns the
		// projection's conditions itself.
		spec := map[string]any{
			"locationClassRef": map[string]any{"name": string(fields.class)},
			"topology":         topology,
		}
		if len(fields.coordinates) > 0 {
			spec["coordinates"] = fields.coordinates
		}
		return spec, true
	}

	spec := map[string]any{
		"locationRef":       map[string]any{"name": locName},
		"locationClassName": string(fields.class),
	}
	if fields.displayName != "" {
		spec["displayName"] = fields.displayName
	}
	// Downstream consumers (e.g. the compute workload webhook) read
	// spec.topology to resolve a location's valid city codes, so an empty
	// topology silently makes every location-scoped deploy fail.
	if len(topology) > 0 {
		spec["topology"] = topology
	}
	return spec, true
}

// availabilityVerdict is the LocationBinding-only aggregate Available verdict.
// The locations.miloapis.com Location carries no equivalent, so callers pass
// nil for that kind.
type availabilityVerdict struct {
	available bool
	reason    string
	message   string
}

// upsertProjection creates or updates one projected object for one location and
// reconciles its status. Metadata/spec are written via CreateOrUpdate; status is
// written separately (and only when it changes) so an already-settled projection
// is a no-op.
//
// mirrored and verdict are mutually exclusive across the two projection kinds:
// only the locations.miloapis.com projection carries the platform Location's
// own conditions, and only LocationBinding carries this reconciler's Available
// verdict. LocationBinding is a different kind with its own status contract,
// read by the network-services operator, so foreign conditions are not
// mirrored onto it.
func (r *LocationBindingReconciler) upsertProjection(
	ctx context.Context,
	consumerClient client.Client,
	gvk schema.GroupVersionKind,
	spec map[string]any,
	locName string,
	fields locationFields,
	mirrored []metav1.Condition,
	verdict *availabilityVerdict,
) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(locName)

	if _, err := controllerutil.CreateOrUpdate(ctx, consumerClient, u, func() error {
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelManagedBy] = labelManagedByValue
		labels[labelLocation] = locName
		labels[labelClass] = string(fields.class)
		delete(labels, labelServiceName)
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

	return r.setProjectionStatus(ctx, consumerClient, u, mirrored, verdict)
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
// dropped from that mirror regardless of verdict, since the platform's own
// Available means something different from either projection's — LocationBinding's
// combined verdict, or nothing at all for the Location kind.
//
// observedGeneration is deliberately not carried across: on a mirrored condition
// it refers to the platform Location's generation, which means nothing against
// the projection's own.
func (r *LocationBindingReconciler) setProjectionStatus(
	ctx context.Context,
	consumerClient client.Client,
	u *unstructured.Unstructured,
	mirrored []metav1.Condition,
	verdict *availabilityVerdict,
) error {
	before := objectConditions(u)

	after := make([]metav1.Condition, 0, len(mirrored)+1)
	for _, c := range mirrored {
		if c.Type == ConditionTypeAvailable {
			continue
		}
		after = append(after, c)
	}

	if verdict != nil {
		status := metav1.ConditionFalse
		if verdict.available {
			status = metav1.ConditionTrue
		}
		// Carry the existing Available forward so SetStatusCondition can keep
		// its lastTransitionTime when the verdict has not moved.
		if prev := apimeta.FindStatusCondition(before, ConditionTypeAvailable); prev != nil {
			after = append(after, *prev)
		}
		apimeta.SetStatusCondition(&after, metav1.Condition{
			Type:    ConditionTypeAvailable,
			Status:  status,
			Reason:  verdict.reason,
			Message: verdict.message,
		})
	}

	if conditionSetsEqual(before, after) {
		return nil
	}
	if err := setObjectConditions(u, after); err != nil {
		return err
	}
	if err := consumerClient.Status().Update(ctx, u, client.FieldOwner(locationBindingFieldManager)); err != nil {
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
func (r *LocationBindingReconciler) mirrorAvailability(
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
func (r *LocationBindingReconciler) prune(
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
			if apimeta.IsNoMatchError(err) {
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

// SetupWithManager registers the reconciler on the multicluster manager.
//
// The primary watch is ServiceEntitlement, scoped to engaged project clusters
// (WithEngageWithProviderClusters), mirroring ServiceEntitlementReconciler.
// The three gates also depend on ServiceAvailability, ServiceConfiguration, and
// Location objects on the root cluster; multicluster-runtime has no clean way
// to translate a root-cluster object event into a project-scoped reconcile
// request, so those gates are picked up by the periodic resync configured via
// RequeueAfter rather than by additional watches.
//
// Reconciles run concurrently across projects. Two passes over the same project
// are still serialized by controller-runtime's per-key locking, so the
// concurrency here only ever puts different projects in flight at once. See
// locationBindingMaxConcurrentReconciles.
func (r *LocationBindingReconciler) SetupWithManager(mgr mcmanager.Manager, rootClient client.Client) error {
	r.rootClient = rootClient
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("location-binding").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		WithOptions(controller.TypedOptions[mcreconcile.Request]{
			MaxConcurrentReconciles: locationBindingMaxConcurrentReconciles,
		}).
		Complete(r)
}
