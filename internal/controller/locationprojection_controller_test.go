// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	projectionProject     = "consumer-proj"
	projectionEntitlement = "compute"
	projectionService     = "compute.miloapis.com"
	projectionConfig      = "compute-miloapis-com"
	projectionLocation    = "us-central1-a"
	projectionClass       = "datum-managed"
)

func projectionScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(LocationGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(LocationGVK.GroupVersion().WithKind("LocationList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(legacyLocationBindingGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(legacyLocationBindingGVK.GroupVersion().WithKind("LocationBindingList"), &unstructured.UnstructuredList{})
	return s
}

func projectionRootClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(projectionScheme()).WithObjects(objs...).Build()
}

func projectionConsumerClient(objs ...client.Object) client.Client {
	location := &unstructured.Unstructured{}
	location.SetGroupVersionKind(LocationGVK)
	legacyBinding := &unstructured.Unstructured{}
	legacyBinding.SetGroupVersionKind(legacyLocationBindingGVK)
	return fake.NewClientBuilder().
		WithScheme(projectionScheme()).
		WithObjects(objs...).
		WithStatusSubresource(location, legacyBinding, &servicesv1alpha1.ServiceAvailability{}, &servicesv1alpha1.ServiceEntitlement{}).
		Build()
}

func activeProjectionEntitlement() *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: projectionEntitlement, UID: types.UID("entitlement-uid")},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: projectionService}},
		Status:     servicesv1alpha1.ServiceEntitlementStatus{Phase: servicesv1alpha1.EntitlementPhaseActive},
	}
}

func publishedProjectionConfig(classes ...servicesv1alpha1.LocationClassName) *servicesv1alpha1.ServiceConfiguration {
	config := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: projectionConfig, UID: types.UID("config-uid")},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: projectionService},
			Phase:      servicesv1alpha1.PhasePublished,
		},
		Status: servicesv1alpha1.ServiceConfigurationStatus{ServiceName: projectionService},
	}
	if len(classes) > 0 {
		config.Spec.Locations = &servicesv1alpha1.ServiceLocationConfig{SupportedClasses: classes}
	}
	return config
}

func availableAt(name string, available bool) *servicesv1alpha1.ServiceAvailability {
	sa := &servicesv1alpha1.ServiceAvailability{
		ObjectMeta: metav1.ObjectMeta{Name: projectionService + "--" + name},
		Spec: servicesv1alpha1.ServiceAvailabilitySpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: projectionService},
			LocationRef: servicesv1alpha1.LocationRef{Name: name},
		},
	}
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&sa.Status.Conditions, metav1.Condition{Type: ConditionTypeAvailable, Status: status, Reason: "Test"})
	return sa
}

func platformLocation(name string, ready bool, class string) *unstructured.Unstructured {
	loc := &unstructured.Unstructured{}
	loc.SetGroupVersionKind(LocationGVK)
	loc.SetName(name)
	_ = unstructured.SetNestedField(loc.Object, class, "spec", "locationClassRef", "name")
	_ = unstructured.SetNestedStringMap(loc.Object, map[string]string{
		"topology.datum.net/city-code": "ORD",
		"topology.datum.net/region":    "us-central1",
	}, "spec", "topology")
	status := string(metav1.ConditionFalse)
	if ready {
		status = string(metav1.ConditionTrue)
	}
	_ = unstructured.SetNestedSlice(loc.Object, []any{map[string]any{
		"type": "Ready", "status": status, "reason": "TestReady", "message": "test",
	}}, "status", "conditions")
	return loc
}

func reconcileProjection(t *testing.T, root, consumer client.Client) (ctrl.Result, error) {
	t.Helper()
	mgr := newTestManager()
	mgr.add(projectionProject, consumer)
	r := &LocationProjectionReconciler{rootClient: root, Manager: mgr, Scheme: projectionScheme()}
	return r.Reconcile(context.Background(), mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: locationProjectionRequestName}},
		ClusterName: projectionProject,
	})
}

func projectedLocation(t *testing.T, c client.Client, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(LocationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, u); err != nil {
		return nil, false
	}
	return u, true
}

func mirroredAvailability(t *testing.T, c client.Client, name string) (*servicesv1alpha1.ServiceAvailability, bool) {
	t.Helper()
	var sa servicesv1alpha1.ServiceAvailability
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &sa); err != nil {
		return nil, false
	}
	return &sa, true
}

func TestLocationProjectionReconciler_ProjectsCanonicalLocationAndAvailability(t *testing.T) {
	root := projectionRootClient(
		publishedProjectionConfig(projectionClass), availableAt(projectionLocation, true),
		platformLocation(projectionLocation, true, projectionClass),
	)
	consumer := projectionConsumerClient(activeProjectionEntitlement())
	if _, err := reconcileProjection(t, root, consumer); err != nil {
		t.Fatal(err)
	}

	loc, ok := projectedLocation(t, consumer, projectionLocation)
	if !ok {
		t.Fatal("projected Location not found")
	}
	class, _, _ := unstructured.NestedString(loc.Object, "spec", "locationClassRef", "name")
	if class != projectionClass {
		t.Errorf("class = %q, want %q", class, projectionClass)
	}
	topology, _, _ := unstructured.NestedStringMap(loc.Object, "spec", "topology")
	if topology["topology.datum.net/city-code"] != "ORD" {
		t.Errorf("topology = %#v", topology)
	}
	if len(loc.GetOwnerReferences()) != 0 {
		t.Errorf("unexpected owner references: %#v", loc.GetOwnerReferences())
	}
	if apimeta.FindStatusCondition(objectConditions(loc), "Ready") == nil {
		t.Error("Ready condition was not mirrored")
	}
	if apimeta.FindStatusCondition(objectConditions(loc), ConditionTypeAvailable) != nil {
		t.Error("Location must not carry service availability")
	}
	if _, ok := mirroredAvailability(t, consumer, projectionService+"--"+projectionLocation); !ok {
		t.Error("ServiceAvailability was not mirrored")
	}
}

func TestLocationProjectionReconciler_RequiresSupportedClassAndAvailability(t *testing.T) {
	tests := []struct {
		name      string
		class     servicesv1alpha1.LocationClassName
		available bool
	}{
		{name: "unsupported class", class: servicesv1alpha1.LocationClassProviderDedicated, available: true},
		{name: "service unavailable", class: projectionClass, available: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := projectionRootClient(publishedProjectionConfig(tt.class), availableAt(projectionLocation, tt.available), platformLocation(projectionLocation, true, projectionClass))
			consumer := projectionConsumerClient(activeProjectionEntitlement())
			if _, err := reconcileProjection(t, root, consumer); err != nil {
				t.Fatal(err)
			}
			if _, ok := projectedLocation(t, consumer, projectionLocation); ok {
				t.Error("unexpected projected Location")
			}
			if _, ok := mirroredAvailability(t, consumer, projectionService+"--"+projectionLocation); ok {
				t.Error("unexpected availability mirror")
			}
		})
	}
}

func TestLocationProjectionReconciler_PrunesStaleObjects(t *testing.T) {
	stale := platformLocation(projectionLocation, true, projectionClass)
	stale.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	mirror := availableAt(projectionLocation, true)
	mirror.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	consumer := projectionConsumerClient(stale, mirror)
	if _, err := reconcileProjection(t, projectionRootClient(), consumer); err != nil {
		t.Fatal(err)
	}
	if _, ok := projectedLocation(t, consumer, projectionLocation); ok {
		t.Error("stale Location was not pruned")
	}
	if _, ok := mirroredAvailability(t, consumer, mirror.Name); ok {
		t.Error("stale ServiceAvailability was not pruned")
	}
}

func TestLocationProjectionReconcilerRemovesLegacyLocationBindings(t *testing.T) {
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(legacyLocationBindingGVK)
	legacy.SetName(projectionLocation)
	legacy.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	unmanaged := &unstructured.Unstructured{}
	unmanaged.SetGroupVersionKind(legacyLocationBindingGVK)
	unmanaged.SetName("unmanaged")
	consumer := projectionConsumerClient(activeProjectionEntitlement(), legacy, unmanaged)
	if _, err := reconcileProjection(t, projectionRootClient(), consumer); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Get(context.Background(), client.ObjectKey{Name: projectionLocation}, legacy); err == nil {
		t.Fatal("legacy LocationBinding was not removed")
	}
	if err := consumer.Get(context.Background(), client.ObjectKey{Name: unmanaged.GetName()}, unmanaged); err != nil {
		t.Fatalf("unmanaged LocationBinding was removed: %v", err)
	}
}

func TestLocationProjectionReconciler_ClearsLegacyMetadata(t *testing.T) {
	stale := platformLocation(projectionLocation, true, projectionClass)
	stale.SetLabels(map[string]string{
		labelManagedBy:                      labelManagedByValue,
		"networking.datumapis.com/location": projectionLocation,
		"networking.datumapis.com/class":    projectionClass,
		labelServiceName:                    projectionService,
	})
	stale.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: servicesv1alpha1.GroupVersion.String(), Kind: "ServiceEntitlement",
		Name: projectionEntitlement, UID: types.UID("entitlement-uid"), Controller: ptr.To(true),
	}})
	_ = unstructured.SetNestedMap(stale.Object, map[string]any{}, "status")
	root := projectionRootClient(publishedProjectionConfig(projectionClass), availableAt(projectionLocation, true), platformLocation(projectionLocation, true, projectionClass))
	consumer := projectionConsumerClient(activeProjectionEntitlement(), stale)
	if _, err := reconcileProjection(t, root, consumer); err != nil {
		t.Fatal(err)
	}
	got, _ := projectedLocation(t, consumer, projectionLocation)
	if len(got.GetOwnerReferences()) != 0 {
		t.Errorf("legacy owner remains: %#v", got.GetOwnerReferences())
	}
	for _, key := range []string{"networking.datumapis.com/location", "networking.datumapis.com/class", labelServiceName} {
		if _, ok := got.GetLabels()[key]; ok {
			t.Errorf("legacy label %q remains", key)
		}
	}
}

func TestLocationProjectionReconciler_RecordsSetupCompletion(t *testing.T) {
	requestedAt := metav1.NewTime(time.Date(2026, 10, 21, 19, 48, 0, 0, time.UTC))
	entitlement := activeProjectionEntitlement()
	entitlement.CreationTimestamp = requestedAt
	for _, typ := range []string{servicesv1alpha1.ConditionTypeReady, servicesv1alpha1.ConditionTypeDependenciesSatisfied, servicesv1alpha1.ConditionTypeProvisioned} {
		apimeta.SetStatusCondition(&entitlement.Status.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "Complete"})
	}
	root := projectionRootClient(publishedProjectionConfig(projectionClass), availableAt(projectionLocation, true), platformLocation(projectionLocation, true, projectionClass))
	consumer := projectionConsumerClient(entitlement)
	result, err := reconcileProjection(t, root, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %s", result.RequeueAfter)
	}
	var got servicesv1alpha1.ServiceEntitlement
	if err := consumer.Get(context.Background(), types.NamespacedName{Name: entitlement.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Setup == nil || got.Status.Setup.FirstCompletedAt == nil {
		t.Fatalf("setup not completed: %#v", got.Status.Setup)
	}
	if !got.Status.Setup.RequestedAt.Equal(&requestedAt) {
		t.Errorf("requestedAt = %s", got.Status.Setup.RequestedAt)
	}
}

func TestProjectionProjectIndex(t *testing.T) {
	index := newProjectionProjectIndex()
	index.Replace("a", []string{"compute"})
	index.Replace("b", []string{"compute", "storage"})
	index.Replace("a", nil)
	got := index.Projects()
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("projects = %#v, want [b]", got)
	}
	if got := index.ProjectsForService("compute"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("compute projects = %#v, want [b]", got)
	}
}

func TestProjectionRootDispatcherTargetsServicesAndDoesNotDropOnFullBuffer(t *testing.T) {
	index := newProjectionProjectIndex()
	index.Replace("compute-a", []string{"compute"})
	index.Replace("compute-b", []string{"compute"})
	index.Replace("storage", []string{"storage"})
	events := make(chan event.TypedGenericEvent[mcreconcile.Request], 1)
	dispatcher := &projectionRootDispatcher{index: index, events: events}
	sa := &servicesv1alpha1.ServiceAvailability{Spec: servicesv1alpha1.ServiceAvailabilitySpec{
		ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"},
	}}

	done := make(chan struct{})
	go func() {
		dispatcher.dispatchObjects(context.Background(), sa)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for len(events) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("dispatcher completed after one delivery; a project invalidation was dropped")
	default:
	}
	first := <-events
	second := <-events
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not complete after buffer capacity became available")
	}
	got := []string{string(first.Object.ClusterName), string(second.Object.ClusterName)}
	if got[0] != "compute-a" || got[1] != "compute-b" {
		t.Fatalf("dispatched projects = %#v, want [compute-a compute-b]", got)
	}

	dispatcher.dispatchObjects(context.Background(), &servicesv1alpha1.ServiceConfiguration{
		Spec: servicesv1alpha1.ServiceConfigurationSpec{ServiceRef: servicesv1alpha1.ServiceReference{Name: "storage"}},
	})
	if got := string((<-events).Object.ClusterName); got != "storage" {
		t.Fatalf("storage event dispatched to %q", got)
	}
}

func TestProjectionRootDispatcherUnionsUpdateTargetsAndFansOutLocations(t *testing.T) {
	index := newProjectionProjectIndex()
	index.Replace("compute", []string{"compute"})
	index.Replace("storage", []string{"storage"})
	events := make(chan event.TypedGenericEvent[mcreconcile.Request], 4)
	dispatcher := &projectionRootDispatcher{index: index, events: events}

	dispatcher.dispatchObjects(context.Background(),
		&servicesv1alpha1.ServiceConfiguration{Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
		}},
		&servicesv1alpha1.ServiceConfiguration{Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "storage"},
		}},
	)
	for _, want := range []string{"compute", "storage"} {
		if got := string((<-events).Object.ClusterName); got != want {
			t.Fatalf("update dispatched to %q, want %q", got, want)
		}
	}

	location := &unstructured.Unstructured{}
	location.SetGroupVersionKind(LocationGVK)
	dispatcher.dispatchObjects(context.Background(), location)
	for _, want := range []string{"compute", "storage"} {
		if got := string((<-events).Object.ClusterName); got != want {
			t.Fatalf("Location dispatched to %q, want %q", got, want)
		}
	}
}

func TestLocationProjectionReconcilerDropsDisengagedProjectFromIndex(t *testing.T) {
	index := newProjectionProjectIndex()
	index.Replace(projectionProject, []string{projectionService})
	r := &LocationProjectionReconciler{Manager: newTestManager(), projects: index}
	result, err := r.Reconcile(context.Background(), mcreconcile.Request{ClusterName: projectionProject})
	if err != nil || result != (ctrl.Result{}) {
		t.Fatalf("reconcile result=%+v err=%v", result, err)
	}
	if got := index.Projects(); len(got) != 0 {
		t.Fatalf("stale projects = %#v, want none", got)
	}
}

func TestProjectionTeardownDeletesOnlyManagedSharedProjections(t *testing.T) {
	managedLocation := platformLocation(projectionLocation, true, projectionClass)
	managedLocation.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	unmanagedLocation := platformLocation("unmanaged", true, projectionClass)
	managedAvailability := availableAt(projectionLocation, true)
	managedAvailability.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	unmanagedAvailability := availableAt("unmanaged", true)
	managedLegacyBinding := &unstructured.Unstructured{}
	managedLegacyBinding.SetGroupVersionKind(legacyLocationBindingGVK)
	managedLegacyBinding.SetName(projectionLocation)
	managedLegacyBinding.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	c := projectionConsumerClient(managedLocation, unmanagedLocation, managedLegacyBinding, managedAvailability, unmanagedAvailability)

	if err := (ProjectionTeardown{}).TeardownConsumer(context.Background(), projectionProject, c, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := projectedLocation(t, c, projectionLocation); ok {
		t.Error("managed Location was not deleted")
	}
	if _, ok := mirroredAvailability(t, c, managedAvailability.Name); ok {
		t.Error("managed ServiceAvailability was not deleted")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: projectionLocation}, managedLegacyBinding); err == nil {
		t.Error("managed legacy LocationBinding was not deleted")
	}
	if _, ok := projectedLocation(t, c, unmanagedLocation.GetName()); !ok {
		t.Error("unmanaged Location was deleted")
	}
	if _, ok := mirroredAvailability(t, c, unmanagedAvailability.Name); !ok {
		t.Error("unmanaged ServiceAvailability was deleted")
	}
}
