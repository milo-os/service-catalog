// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// locationBindingGVK is the consumer-facing projection the reconciler writes
// into a project's virtual control plane. LocationBinding is owned by the
// network-services operator (networking.datumapis.com); the typed Go types
// are not yet available in go.miloapis.com/milo, so it is read and written as
// an unstructured object — the same deliberate choice the ServiceAvailability
// reconciler makes for Location.
var locationBindingGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha",
	Kind:    "LocationBinding",
}

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
	// lookup; labelServiceName records which service projected the binding.
	labelLocation    = "networking.datumapis.com/location"
	labelClass       = "networking.datumapis.com/class"
	labelServiceName = "services.miloapis.com/service-name"

	// locationBindingFieldManager identifies writes this reconciler makes to
	// LocationBinding objects.
	locationBindingFieldManager = "services-operator-locationbinding"

	// reasonAllGatesOpen is the Available=True reason: class supported,
	// Location Ready, and ServiceAvailability Available.
	reasonAllGatesOpen = "AllGatesOpen"

	// reasonLocationClassNotSupported is the Available=False reason when the
	// Location's class is not in the active ServiceConfiguration's
	// supportedClasses (gate 1 closed).
	reasonLocationClassNotSupported = "LocationClassNotSupported"
	// reasonLocationNotReady ("LocationNotReady", gate 2) is shared with the
	// ServiceAvailability reconciler; it is declared there.
)

// LocationBindingReconciler projects platform Locations into entitled projects
// as consumer-facing, cluster-scoped LocationBinding objects. It is the
// tier-2 half of the location two-tier discovery model and the place where the
// three location gates are combined:
//
//	gate 1: the Location's class is in the active ServiceConfiguration's
//	        spec.locations.supportedClasses
//	gate 2: the Location's status.conditions[Ready] is True
//	gate 3: a ServiceAvailability for (service, location) reports
//	        status.conditions[Available] = True
//
// It runs on the multicluster manager so each reconcile is scoped to one
// engaged project cluster (req.ClusterName), where it reads the project's
// ServiceEntitlements and writes LocationBindings. ServiceAvailability,
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
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=locations,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=locationbindings,verbs=get;list;watch;create;update;patch;delete
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

	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(ctx, req.NamespacedName, &entitlement); err != nil {
		if apierrors.IsNotFound(err) {
			// The entitlement is gone; its LocationBindings carry a controller
			// owner reference to it and are reclaimed by garbage collection.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceEntitlement: %w", err)
	}

	// Only an Active entitlement projects locations. Anything else (pending
	// approval, rejected, deleting) tears its bindings down.
	if !entitlement.DeletionTimestamp.IsZero() ||
		entitlement.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		if err := r.cleanupBindings(ctx, consumerClient, entitlement.UID, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	serviceRefName := entitlement.Spec.ServiceRef.Name

	// Gate-source 1: the active (latest Published) ServiceConfiguration for the
	// service supplies spec.locations.supportedClasses. Without it there is
	// nothing to evaluate; requeue and try again later.
	sc, err := r.latestPublishedConfiguration(ctx, serviceRefName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sc == nil {
		logger.V(1).Info("no published ServiceConfiguration for service yet, requeuing", "service", serviceRefName)
		return ctrl.Result{RequeueAfter: locationBindingResyncInterval}, nil
	}

	serviceName := sc.Status.ServiceName
	if serviceName == "" {
		serviceName = serviceRefName
	}

	if sc.Spec.Locations == nil || len(sc.Spec.Locations.SupportedClasses) == 0 {
		// The service version declares no location classes; nothing is
		// projectable. Ensure no stale bindings remain.
		if err := r.cleanupBindings(ctx, consumerClient, entitlement.UID, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: locationBindingResyncInterval}, nil
	}
	supported := make(map[servicesv1alpha1.LocationClassName]struct{}, len(sc.Spec.Locations.SupportedClasses))
	for _, c := range sc.Spec.Locations.SupportedClasses {
		supported[c] = struct{}{}
	}

	// Gate-source 3: the ServiceAvailability records for this service. Only
	// availabilities reporting Available=True back a binding at all; a closed
	// gate 3 means the service is not operational at that location, so no
	// binding is projected (and any prior one is pruned below).
	var saList servicesv1alpha1.ServiceAvailabilityList
	if err := r.rootClient.List(ctx, &saList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceAvailabilities: %w", err)
	}

	desired := make(map[string]struct{})
	for i := range saList.Items {
		sa := &saList.Items[i]
		if sa.Spec.ServiceRef.Name != serviceRefName {
			continue
		}
		if !apimeta.IsStatusConditionTrue(sa.Status.Conditions, ConditionTypeAvailable) {
			continue
		}

		locName := sa.Spec.LocationRef.Name

		// Gate-source 2: load the referenced Location. A transient read failure
		// must requeue without disturbing existing bindings — never flip a
		// binding to unavailable on a blip.
		loc := &unstructured.Unstructured{}
		loc.SetGroupVersionKind(locationGVK)
		locKey := types.NamespacedName{Name: locName}
		if err := r.rootClient.Get(ctx, locKey, loc); err != nil {
			if apierrors.IsNotFound(err) {
				// The Location vanished out from under an Available record;
				// treat as a closed gate and let the prune remove any binding.
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get Location %q: %w", locName, err)
		}

		fields := extractLocationFields(loc)

		available, reason, message := evaluateGates(fields.class, supported, locationReady(loc), locName)
		if err := r.upsertBinding(ctx, consumerClient, &entitlement, serviceName, locName, fields, available, reason, message); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to upsert LocationBinding %q: %w", locName, err)
		}
		desired[locName] = struct{}{}
	}

	// Prune any binding we own that is no longer backed by an Available
	// ServiceAvailability (gate 3 closed, or the Location/SA was removed).
	if err := r.cleanupBindings(ctx, consumerClient, entitlement.UID, desired); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled location bindings", "service", serviceRefName, "bindings", len(desired))
	return ctrl.Result{RequeueAfter: locationBindingResyncInterval}, nil
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
	// topology mirrors the referenced Location's spec.topology verbatim. The
	// network-services LocationBinding API expects spec.topology to carry the
	// well-known keys (e.g. topology.datum.net/city-code,
	// topology.datum.net/region) that downstream consumers like the compute
	// workload webhook read to resolve a binding's valid city codes.
	topology map[string]string
}

// extractLocationFields pulls the projection fields out of an unstructured
// Location. Every field is best-effort: a Location missing optional metadata
// still yields a binding, just with the corresponding fields omitted.
func extractLocationFields(loc *unstructured.Unstructured) locationFields {
	var f locationFields
	if s, _, _ := unstructured.NestedString(loc.Object, "spec", "locationClassName"); s != "" {
		f.class = servicesv1alpha1.LocationClassName(s)
	}
	f.displayName, _, _ = unstructured.NestedString(loc.Object, "spec", "displayName")
	// City, region, and other location attributes are not first-class spec
	// fields on the NSO Location; they are carried in spec.topology under
	// datum.net topology keys. Mirror the whole map onto the binding verbatim.
	f.topology, _, _ = unstructured.NestedStringMap(loc.Object, "spec", "topology")
	return f
}

// upsertBinding creates or updates the LocationBinding for one location and
// reconciles its Available condition. Metadata/spec are written via
// CreateOrUpdate; the status condition is written separately (and only when it
// changes) so an already-settled binding is a no-op.
func (r *LocationBindingReconciler) upsertBinding(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	serviceName string,
	locName string,
	fields locationFields,
	available bool,
	reason, message string,
) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(locationBindingGVK)
	u.SetName(locName)

	if _, err := controllerutil.CreateOrUpdate(ctx, consumerClient, u, func() error {
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelManagedBy] = labelManagedByValue
		labels[labelLocation] = locName
		labels[labelClass] = string(fields.class)
		labels[labelServiceName] = serviceName
		u.SetLabels(labels)

		// The cluster-scoped ServiceEntitlement owns its projected bindings so
		// that deleting the entitlement garbage-collects them.
		if err := controllerutil.SetControllerReference(entitlement, u, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		spec := map[string]any{
			"locationRef":       map[string]any{"name": locName},
			"locationClassName": string(fields.class),
		}
		if fields.displayName != "" {
			spec["displayName"] = fields.displayName
		}
		// Mirror the referenced Location's spec.topology onto the binding.
		// Downstream consumers (e.g. the compute workload webhook) read
		// spec.topology to resolve a binding's valid city codes, so an empty
		// topology silently makes every location-scoped deploy fail.
		if len(fields.topology) > 0 {
			topology := make(map[string]any, len(fields.topology))
			for k, v := range fields.topology {
				topology[k] = v
			}
			spec["topology"] = topology
		}
		return unstructured.SetNestedMap(u.Object, spec, "spec")
	}); err != nil {
		return err
	}

	return r.setBindingAvailable(ctx, consumerClient, u, available, reason, message)
}

// setBindingAvailable updates the binding's Available condition, writing to the
// status subresource only when the condition actually changes.
func (r *LocationBindingReconciler) setBindingAvailable(
	ctx context.Context,
	consumerClient client.Client,
	u *unstructured.Unstructured,
	available bool,
	reason, message string,
) error {
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}

	before := bindingConditions(u)
	after := append([]metav1.Condition(nil), before...)
	apimeta.SetStatusCondition(&after, metav1.Condition{
		Type:    ConditionTypeAvailable,
		Status:  status,
		Reason:  reason,
		Message: message,
	})

	if conditionsEqual(before, after, ConditionTypeAvailable) {
		return nil
	}
	if err := setBindingConditions(u, after); err != nil {
		return err
	}
	if err := consumerClient.Status().Update(ctx, u, client.FieldOwner(locationBindingFieldManager)); err != nil {
		return fmt.Errorf("failed to update LocationBinding status: %w", err)
	}
	return nil
}

// cleanupBindings deletes LocationBindings owned by the entitlement that are
// not in the keep set. A nil keep set deletes every owned binding (used when
// the entitlement is no longer Active).
func (r *LocationBindingReconciler) cleanupBindings(
	ctx context.Context,
	consumerClient client.Client,
	entitlementUID types.UID,
	keep map[string]struct{},
) error {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(locationBindingGVK.GroupVersion().WithKind("LocationBindingList"))
	if err := consumerClient.List(ctx, &list,
		client.MatchingLabelsSelector{Selector: managedByFanoutSelector},
	); err != nil {
		if apimeta.IsNoMatchError(err) {
			// The LocationBinding CRD is not installed in this project cluster;
			// nothing to prune.
			return nil
		}
		return fmt.Errorf("failed to list LocationBindings: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if !ownedBy(item.GetOwnerReferences(), entitlementUID) {
			continue
		}
		if _, ok := keep[item.GetName()]; ok {
			continue
		}
		if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete stale LocationBinding %q: %w", item.GetName(), err)
		}
	}
	return nil
}

// bindingConditions parses status.conditions off an unstructured binding into
// typed conditions. LastTransitionTime is preserved so apimeta.SetStatusCondition
// can decide whether a write is needed.
func bindingConditions(u *unstructured.Unstructured) []metav1.Condition {
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

// setBindingConditions writes typed conditions back onto an unstructured
// binding's status.conditions.
func setBindingConditions(u *unstructured.Unstructured, conds []metav1.Condition) error {
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

// SetupWithManager registers the reconciler on the multicluster manager.
//
// The primary watch is ServiceEntitlement, scoped to engaged project clusters
// (WithEngageWithProviderClusters), mirroring ServiceEntitlementReconciler.
// The three gates also depend on ServiceAvailability, ServiceConfiguration, and
// Location objects on the root cluster; multicluster-runtime has no clean way
// to translate a root-cluster object event into a project-scoped reconcile
// request, so those gates are picked up by the periodic resync configured via
// RequeueAfter rather than by additional watches.
func (r *LocationBindingReconciler) SetupWithManager(mgr mcmanager.Manager, rootClient client.Client) error {
	r.rootClient = rootClient
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("location-binding").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(r)
}
