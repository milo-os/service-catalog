// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	lbConsumerProject = "consumer-proj"
	lbEntitlement     = "compute"
	lbServiceName     = "compute.miloapis.com"
	lbConfigName      = "compute-miloapis-com"
	lbLoc             = "us-central1-a"
	lbClass           = "datum-managed"
	lbEntitlementUID  = types.UID("ent-compute-uid")
)

// bindingScheme registers the services types plus the foreign Location and
// LocationBinding GVKs as unstructured so the fake clients can serve gate
// reads and projection writes.
func bindingScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	registerLocationGVKs(s)
	s.AddKnownTypeWithName(locationBindingGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(locationBindingGVK.GroupVersion().WithKind("LocationBindingList"), &unstructured.UnstructuredList{})
	return s
}

// newBindingRootClient builds the root-cluster client. ServiceAvailability and
// ServiceConfiguration are read-only here, so they are deliberately left out of
// the status subresource set — that preserves the status we seed via WithObjects.
func newBindingRootClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(bindingScheme()).
		WithObjects(objs...).
		Build()
}

// newBindingConsumerClient builds a project VCP client with LocationBinding
// status-subresource support. ServiceEntitlement is left out of the subresource
// set so the seeded status.phase survives object creation.
func newBindingConsumerClient(objs ...client.Object) client.Client {
	subresourced := make([]client.Object, 0, len(projectionGVKs))
	for _, gvk := range projectionGVKs {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		subresourced = append(subresourced, u)
	}
	return fake.NewClientBuilder().
		WithScheme(bindingScheme()).
		WithObjects(objs...).
		WithStatusSubresource(subresourced...).
		Build()
}

func newActiveEntitlement() *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: lbEntitlement, UID: lbEntitlementUID},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: lbServiceName},
		},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			Phase: servicesv1alpha1.EntitlementPhaseActive,
		},
	}
}

func newPublishedConfigWithClasses(classes ...servicesv1alpha1.LocationClassName) *servicesv1alpha1.ServiceConfiguration {
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: lbConfigName},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: lbServiceName},
			Phase:      servicesv1alpha1.PhasePublished,
		},
		Status: servicesv1alpha1.ServiceConfigurationStatus{ServiceName: lbServiceName},
	}
	if len(classes) > 0 {
		sc.Spec.Locations = &servicesv1alpha1.ServiceLocationConfig{SupportedClasses: classes}
	}
	return sc
}

func newAvailabilityWithCondition(locName string, available bool) *servicesv1alpha1.ServiceAvailability {
	sa := &servicesv1alpha1.ServiceAvailability{
		ObjectMeta: metav1.ObjectMeta{Name: lbServiceName + "--" + locName},
		Spec: servicesv1alpha1.ServiceAvailabilitySpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: lbServiceName},
			LocationRef: servicesv1alpha1.LocationRef{Name: locName},
		},
	}
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&sa.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeAvailable,
		Status:  status,
		Reason:  "Test",
		Message: "test",
	})
	return sa
}

// newClassyLocation builds a Location carrying a class, city/region, and a
// display name, reusing newLocation (from serviceavailability_controller_test.go)
// for the Ready condition.
func newClassyLocation(name string, ready bool, class string) *unstructured.Unstructured {
	loc := newLocation(name, ready)
	_ = unstructured.SetNestedField(loc.Object, class, "spec", "locationClassName")
	return withLocationDetail(loc)
}

// newClassyMiloLocation is newClassyLocation against locations.miloapis.com,
// where the class is a reference rather than a flat name and displayName has no
// equivalent.
func newClassyMiloLocation(name string, ready bool, class string) *unstructured.Unstructured {
	loc := newMiloLocation(name, ready)
	_ = unstructured.SetNestedField(loc.Object, class, "spec", "locationClassRef", "name")
	_ = unstructured.SetNestedStringMap(loc.Object, map[string]string{
		"topology.datum.net/city-code": "ORD",
		"topology.datum.net/region":    "us-central1",
	}, "spec", "topology")
	return loc
}

func withLocationDetail(loc *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(loc.Object, "Chicago", "spec", "displayName")
	// City and region live in the NSO Location's spec.topology map, not as
	// first-class spec fields.
	_ = unstructured.SetNestedStringMap(loc.Object, map[string]string{
		"topology.datum.net/city-code": "ORD",
		"topology.datum.net/region":    "us-central1",
	}, "spec", "topology")
	return loc
}

func existingBinding(locName string, ownerUID types.UID) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(locationBindingGVK)
	u.SetName(locName)
	u.SetLabels(map[string]string{
		labelManagedBy:   labelManagedByValue,
		labelServiceName: lbServiceName,
	})
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         servicesv1alpha1.GroupVersion.String(),
		Kind:               "ServiceEntitlement",
		Name:               lbEntitlement,
		UID:                ownerUID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}})
	return u
}

// reconcileBindings runs one reconcile pass for the consumer-project
// entitlement.
func reconcileBindings(t *testing.T, rootClient, consumerClient client.Client) (ctrl.Result, error) {
	t.Helper()
	return reconcileBindingsFrom(t, rootClient, consumerClient, legacyLocationGVK)
}

// reconcileBindingsFrom runs one reconcile pass reading locations from the
// given source.
func reconcileBindingsFrom(t *testing.T, rootClient, consumerClient client.Client, locationGVK schema.GroupVersionKind) (ctrl.Result, error) {
	t.Helper()
	mgr := newTestManager()
	mgr.add(lbConsumerProject, consumerClient)
	r := &LocationBindingReconciler{
		rootClient:  rootClient,
		Manager:     mgr,
		Scheme:      bindingScheme(),
		LocationGVK: locationGVK,
	}
	req := mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: lbEntitlement}},
		ClusterName: lbConsumerProject,
	}
	return r.Reconcile(context.Background(), req)
}

func getBinding(t *testing.T, c client.Client, locName string) (*unstructured.Unstructured, bool) {
	t.Helper()
	return getProjection(t, c, locationBindingGVK, locName)
}

func getProjectedLocation(t *testing.T, c client.Client, locName string) (*unstructured.Unstructured, bool) {
	t.Helper()
	return getProjection(t, c, projectedLocationGVK, locName)
}

func getProjection(t *testing.T, c client.Client, gvk schema.GroupVersionKind, locName string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: locName}, u)
	if err != nil {
		return nil, false
	}
	return u, true
}

func assertBindingAvailable(t *testing.T, u *unstructured.Unstructured, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	cond := apimeta.FindStatusCondition(objectConditions(u), ConditionTypeAvailable)
	if cond == nil {
		t.Fatalf("Available condition not set on binding %q", u.GetName())
	}
	if cond.Status != wantStatus {
		t.Errorf("binding %q Available status = %q, want %q (reason %q)", u.GetName(), cond.Status, wantStatus, cond.Reason)
	}
	if cond.Reason != wantReason {
		t.Errorf("binding %q Available reason = %q, want %q", u.GetName(), cond.Reason, wantReason)
	}
}

func TestLocationBindingReconciler_AllGatesOpen(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	u, ok := getBinding(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected LocationBinding %q to exist", lbLoc)
	}
	assertBindingAvailable(t, u, metav1.ConditionTrue, reasonAllGatesOpen)

	// Spec and labels are projected from the Location.
	class, _, _ := unstructured.NestedString(u.Object, "spec", "locationClassName")
	if class != lbClass {
		t.Errorf("spec.locationClassName = %q, want %q", class, lbClass)
	}
	if got := u.GetLabels()[labelServiceName]; got != lbServiceName {
		t.Errorf("service-name label = %q, want %q", got, lbServiceName)
	}
	if got := u.GetLabels()[labelClass]; got != lbClass {
		t.Errorf("class label = %q, want %q", got, lbClass)
	}
	// The Location's spec.topology must be mirrored verbatim onto the binding;
	// downstream consumers (e.g. the compute workload webhook) read these keys
	// to resolve the binding's valid city codes, so an empty topology breaks
	// location-scoped deploys.
	topology, found, err := unstructured.NestedStringMap(u.Object, "spec", "topology")
	if err != nil || !found {
		t.Fatalf("spec.topology not set on binding (found=%v, err=%v)", found, err)
	}
	if got := topology["topology.datum.net/city-code"]; got != "ORD" {
		t.Errorf("spec.topology[city-code] = %q, want ORD", got)
	}
	if got := topology["topology.datum.net/region"]; got != "us-central1" {
		t.Errorf("spec.topology[region] = %q, want us-central1", got)
	}
	if !ownedBy(u.GetOwnerReferences(), lbEntitlementUID) {
		t.Errorf("binding is not owned by the entitlement")
	}
}

func TestLocationBindingReconciler_ClassNotSupported(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(servicesv1alpha1.LocationClassProviderDedicated), // does not include datum-managed
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	u, ok := getBinding(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected LocationBinding %q to exist with Available=False", lbLoc)
	}
	assertBindingAvailable(t, u, metav1.ConditionFalse, reasonLocationClassNotSupported)
}

func TestLocationBindingReconciler_LocationNotReady(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, false, lbClass), // not Ready
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	u, ok := getBinding(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected LocationBinding %q to exist with Available=False", lbLoc)
	}
	assertBindingAvailable(t, u, metav1.ConditionFalse, reasonLocationNotReady)
}

func TestLocationBindingReconciler_ServiceNotAvailable(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, false), // gate 3 closed
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, ok := getBinding(t, consumerClient, lbLoc); ok {
		t.Fatalf("expected no LocationBinding for a non-Available service")
	}
}

// TestLocationBindingReconciler_GateCloses verifies that a binding which is
// projected while gate 3 is open is pruned once the ServiceAvailability flips
// to not-Available.
func TestLocationBindingReconciler_GateCloses(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, ok := getBinding(t, consumerClient, lbLoc); !ok {
		t.Fatalf("expected binding after first reconcile")
	}

	// Flip gate 3 closed on the root cluster.
	var sa servicesv1alpha1.ServiceAvailability
	if err := rootClient.Get(context.Background(), types.NamespacedName{Name: lbServiceName + "--" + lbLoc}, &sa); err != nil {
		t.Fatalf("get SA: %v", err)
	}
	apimeta.SetStatusCondition(&sa.Status.Conditions, metav1.Condition{
		Type: ConditionTypeAvailable, Status: metav1.ConditionFalse, Reason: "Test", Message: "down",
	})
	if err := rootClient.Update(context.Background(), &sa); err != nil {
		t.Fatalf("update SA: %v", err)
	}

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, ok := getBinding(t, consumerClient, lbLoc); ok {
		t.Fatalf("expected binding to be pruned after gate 3 closed")
	}
}

func TestLocationBindingReconciler_EntitlementNotActive(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	ent := newActiveEntitlement()
	ent.Status.Phase = servicesv1alpha1.EntitlementPhasePendingApproval
	consumerClient := newBindingConsumerClient(ent, existingBinding(lbLoc, lbEntitlementUID))

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, ok := getBinding(t, consumerClient, lbLoc); ok {
		t.Fatalf("expected pre-existing binding to be cleaned up for a non-Active entitlement")
	}
}

// TestLocationBindingReconciler_Idempotent locks in patch-only-when-changed: a
// second reconcile over a settled binding must not bump its resourceVersion.
func TestLocationBindingReconciler_Idempotent(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first, ok := getBinding(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected binding after first reconcile")
	}

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	second, _ := getBinding(t, consumerClient, lbLoc)
	if second.GetResourceVersion() != first.GetResourceVersion() {
		t.Errorf("second reconcile mutated binding (resourceVersion %s -> %s); expected a no-op",
			first.GetResourceVersion(), second.GetResourceVersion())
	}
}

func TestLocationBindingReconciler_ProjectsLocation(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	u, ok := getProjectedLocation(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected projected Location %q to exist", lbLoc)
	}
	assertBindingAvailable(t, u, metav1.ConditionTrue, reasonAllGatesOpen)

	class, _, _ := unstructured.NestedString(u.Object, "spec", "locationClassRef", "name")
	if class != lbClass {
		t.Errorf("spec.locationClassRef.name = %q, want %q", class, lbClass)
	}
	if project, found, _ := unstructured.NestedString(u.Object, "spec", "locationClassRef", "project"); found {
		t.Errorf("spec.locationClassRef.project = %q, want unset", project)
	}
	topology, found, err := unstructured.NestedStringMap(u.Object, "spec", "topology")
	if err != nil || !found {
		t.Fatalf("spec.topology not set on projected Location (found=%v, err=%v)", found, err)
	}
	if got := topology["topology.datum.net/city-code"]; got != "ORD" {
		t.Errorf("spec.topology[city-code] = %q, want ORD", got)
	}
	if !ownedBy(u.GetOwnerReferences(), lbEntitlementUID) {
		t.Errorf("projected Location is not owned by the entitlement")
	}
	// The binding is still written until the network-services operator moves
	// off it.
	if _, ok := getBinding(t, consumerClient, lbLoc); !ok {
		t.Errorf("expected LocationBinding %q to still be written alongside the Location", lbLoc)
	}
}

// With the source set to the locations service, projections are driven by that
// group, including the class read from spec.locationClassRef.
func TestLocationBindingReconciler_ReadsConfiguredSource(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyMiloLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindingsFrom(t, rootClient, consumerClient, miloLocationGVK); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	u, ok := getProjectedLocation(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected projected Location %q to exist", lbLoc)
	}
	assertBindingAvailable(t, u, metav1.ConditionTrue, reasonAllGatesOpen)

	b, ok := getBinding(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected LocationBinding %q to exist", lbLoc)
	}
	if got := b.GetLabels()[labelClass]; got != lbClass {
		t.Errorf("class label = %q, want %q", got, lbClass)
	}
}

// A location in the group that is not configured is not read, so no projection
// is made for it. Nothing resolves across groups behind the operator's back.
func TestLocationBindingReconciler_IgnoresUnconfiguredSource(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindingsFrom(t, rootClient, consumerClient, miloLocationGVK); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, ok := getBinding(t, consumerClient, lbLoc); ok {
		t.Errorf("expected no LocationBinding: the location lives in a group that is not the configured source")
	}
	if _, ok := getProjectedLocation(t, consumerClient, lbLoc); ok {
		t.Errorf("expected no projected Location: the location lives in a group that is not the configured source")
	}
}

// A configured source the control plane does not serve makes every location
// look absent. The reconcile must fail rather than treat that as "no locations"
// and tear down every projection an entitled project already has.
func TestLocationBindingReconciler_SourceNotServedKeepsProjections(t *testing.T) {
	rootClient := fake.NewClientBuilder().
		WithScheme(bindingScheme()).
		WithObjects(
			newPublishedConfigWithClasses(lbClass),
			newAvailabilityWithCondition(lbLoc, true),
			newClassyLocation(lbLoc, true, lbClass),
		).
		Build()
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("setup reconcile: %v", err)
	}
	if _, ok := getBinding(t, consumerClient, lbLoc); !ok {
		t.Fatalf("setup: expected LocationBinding %q", lbLoc)
	}

	// Now read through a root client that answers the configured source the
	// way a control plane without that CRD does.
	unserved := interceptor.NewClient(rootClient, noMatchOn(miloLocationGVK))
	_, err := reconcileBindingsFrom(t, unserved, consumerClient, miloLocationGVK)
	if err == nil {
		t.Fatalf("expected an error when the configured location source is not served")
	}
	if !errors.Is(err, errLocationSourceUnavailable) {
		t.Errorf("error = %v, want it to wrap errLocationSourceUnavailable", err)
	}

	if _, ok := getBinding(t, consumerClient, lbLoc); !ok {
		t.Errorf("LocationBinding %q was pruned because the location source was unreachable", lbLoc)
	}
	if _, ok := getProjectedLocation(t, consumerClient, lbLoc); !ok {
		t.Errorf("projected Location %q was pruned because the location source was unreachable", lbLoc)
	}
}

// A source Location with no topology cannot satisfy the locations.miloapis.com
// schema, which requires it. The binding is still projected.
func TestLocationBindingReconciler_SkipsLocationWithoutTopology(t *testing.T) {
	loc := newLocation(lbLoc, true)
	_ = unstructured.SetNestedField(loc.Object, lbClass, "spec", "locationClassName")

	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		loc,
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, ok := getProjectedLocation(t, consumerClient, lbLoc); ok {
		t.Errorf("expected no projected Location for a source location with no topology")
	}
	if _, ok := getBinding(t, consumerClient, lbLoc); !ok {
		t.Errorf("expected LocationBinding %q to be projected regardless", lbLoc)
	}
}

// An entitlement that stops being Active tears down every projection kind, not just the
// binding.
func TestLocationBindingReconciler_PrunesEveryProjection(t *testing.T) {
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		newClassyLocation(lbLoc, true, lbClass),
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getProjectedLocation(t, consumerClient, lbLoc); !ok {
		t.Fatalf("setup: expected projected Location %q", lbLoc)
	}

	ent := newActiveEntitlement()
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: lbEntitlement}, ent); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	ent.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	if err := consumerClient.Update(context.Background(), ent); err != nil {
		t.Fatalf("update entitlement: %v", err)
	}

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getProjectedLocation(t, consumerClient, lbLoc); ok {
		t.Errorf("expected projected Location %q to be pruned", lbLoc)
	}
	if _, ok := getBinding(t, consumerClient, lbLoc); ok {
		t.Errorf("expected LocationBinding %q to be pruned", lbLoc)
	}
}

// withCondition sets a condition on an unstructured Location, preserving any
// already present.
func withCondition(loc *unstructured.Unstructured, condType, status, reason, message string) *unstructured.Unstructured {
	conds := objectConditions(loc)
	apimeta.SetStatusCondition(&conds, metav1.Condition{
		Type:    condType,
		Status:  metav1.ConditionStatus(status),
		Reason:  reason,
		Message: message,
	})
	_ = setObjectConditions(loc, conds)
	return loc
}

func findProjectedCondition(t *testing.T, c client.Client, locName, condType string) *metav1.Condition {
	t.Helper()
	u, ok := getProjectedLocation(t, c, locName)
	if !ok {
		t.Fatalf("expected projected Location %q to exist", locName)
	}
	return apimeta.FindStatusCondition(objectConditions(u), condType)
}

// The platform Location's own conditions reach the consumer. Available alone
// says a location is unusable but never which gate is shut, and Ready is only
// ever written on the platform copy a consumer cannot read.
func TestLocationBindingReconciler_MirrorsLocationStatus(t *testing.T) {
	loc := withCondition(newClassyLocation(lbLoc, true, lbClass),
		"Ready", string(metav1.ConditionTrue), "Ready", "Location is serving.")
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		loc,
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ready := findProjectedCondition(t, consumerClient, lbLoc, "Ready")
	if ready == nil {
		t.Fatalf("Ready condition was not mirrored onto the projected Location")
	}
	if ready.Status != metav1.ConditionTrue || ready.Reason != "Ready" {
		t.Errorf("mirrored Ready = %q/%q, want True/Ready", ready.Status, ready.Reason)
	}
	if ready.Message != "Location is serving." {
		t.Errorf("mirrored Ready message = %q, want the platform message", ready.Message)
	}

	u, _ := getProjectedLocation(t, consumerClient, lbLoc)
	assertBindingAvailable(t, u, metav1.ConditionTrue, reasonAllGatesOpen)
}

// A status change on the platform Location reaches an already-projected copy;
// the projection is not create-only.
func TestLocationBindingReconciler_MirroredStatusTracksSource(t *testing.T) {
	loc := withCondition(newClassyLocation(lbLoc, true, lbClass),
		"Ready", string(metav1.ConditionTrue), "Ready", "Location is serving.")
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		loc,
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(legacyLocationGVK)
	if err := rootClient.Get(context.Background(), types.NamespacedName{Name: lbLoc}, current); err != nil {
		t.Fatalf("get location: %v", err)
	}
	withCondition(current, "Ready", string(metav1.ConditionFalse), "MissingTopology", "No city code.")
	if err := rootClient.Update(context.Background(), current); err != nil {
		t.Fatalf("update location: %v", err)
	}

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	ready := findProjectedCondition(t, consumerClient, lbLoc, "Ready")
	if ready == nil {
		t.Fatalf("Ready condition missing from the projected Location")
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != "MissingTopology" {
		t.Errorf("mirrored Ready = %q/%q, want False/MissingTopology", ready.Status, ready.Reason)
	}
}

// Available on the projection is this reconciler's combined verdict over three
// gates. A platform Location carrying its own Available must not displace it.
func TestLocationBindingReconciler_SourceAvailableDoesNotOverrideVerdict(t *testing.T) {
	loc := withCondition(newClassyLocation(lbLoc, true, lbClass),
		ConditionTypeAvailable, string(metav1.ConditionFalse), "SomethingElse", "not this reconciler's verdict")
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		loc,
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	u, _ := getProjectedLocation(t, consumerClient, lbLoc)
	assertBindingAvailable(t, u, metav1.ConditionTrue, reasonAllGatesOpen)
}

// LocationBinding has its own status contract, read by the network-services
// operator. Platform conditions are mirrored onto the Location projection only.
func TestLocationBindingReconciler_DoesNotMirrorOntoBinding(t *testing.T) {
	loc := withCondition(newClassyLocation(lbLoc, true, lbClass),
		"Ready", string(metav1.ConditionTrue), "Ready", "Location is serving.")
	rootClient := newBindingRootClient(
		newPublishedConfigWithClasses(lbClass),
		newAvailabilityWithCondition(lbLoc, true),
		loc,
	)
	consumerClient := newBindingConsumerClient(newActiveEntitlement())

	if _, err := reconcileBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	binding, ok := getBinding(t, consumerClient, lbLoc)
	if !ok {
		t.Fatalf("expected LocationBinding %q to exist", lbLoc)
	}
	if c := apimeta.FindStatusCondition(objectConditions(binding), "Ready"); c != nil {
		t.Errorf("Ready was mirrored onto the LocationBinding, want it left alone")
	}
	assertBindingAvailable(t, binding, metav1.ConditionTrue, reasonAllGatesOpen)
}
