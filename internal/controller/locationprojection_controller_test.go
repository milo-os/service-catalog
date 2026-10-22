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

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
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
	_ = resourcemanagerv1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(LocationGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(LocationGVK.GroupVersion().WithKind("LocationList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(legacyLocationBindingGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(legacyLocationBindingGVK.GroupVersion().WithKind("LocationBindingList"), &unstructured.UnstructuredList{})
	return s
}

func projectionRootClient(objs ...client.Object) client.Client {
	hasProject := false
	for _, obj := range objs {
		if project, ok := obj.(*resourcemanagerv1alpha1.Project); ok && project.Name == projectionProject {
			hasProject = true
			break
		}
	}
	if !hasProject {
		objs = append(objs, &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectionProject}})
	}
	return projectionRootClientWithoutProject(objs...)
}

func projectionRootClientWithoutProject(objs ...client.Object) client.Client {
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
	r := &LocationProjectionReconciler{rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: projectionScheme()}
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

func TestLocationProjectionReconcilerDoesNotDropProjectOnCacheMiss(t *testing.T) {
	cacheClient := projectionRootClientWithoutProject(
		publishedProjectionConfig(projectionClass),
		availableAt(projectionLocation, true),
		platformLocation(projectionLocation, true, projectionClass),
	)
	apiReader := projectionRootClient()
	consumer := projectionConsumerClient(activeProjectionEntitlement())
	mgr := newTestManager()
	mgr.add(projectionProject, consumer)
	index := newProjectionProjectIndex()
	r := &LocationProjectionReconciler{
		rootClient: cacheClient, rootAPIReader: apiReader, Manager: mgr, Scheme: projectionScheme(), projects: index,
	}

	if _, err := r.Reconcile(context.Background(), mcreconcile.Request{ClusterName: projectionProject}); err != nil {
		t.Fatal(err)
	}
	if _, ok := projectedLocation(t, consumer, projectionLocation); !ok {
		t.Fatal("cache miss dropped location projection for a live Project")
	}
	if got := index.Projects(); len(got) != 1 || got[0] != projectionProject {
		t.Fatalf("project index = %#v, want live project retained", got)
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

func TestLocationProjectionReconciler_AggregatesDependencyChainInOnePass(t *testing.T) {
	services := []*servicesv1alpha1.Service{
		setupService("compute", "compute.miloapis.com", "networking"),
		setupService("networking", "networking.miloapis.com", "ipam"),
		setupService("ipam", "ipam.miloapis.com"),
	}
	// Deliberately put parents before dependencies, the order that previously
	// required one controller event per level to converge.
	entitlements := []servicesv1alpha1.ServiceEntitlement{
		*setupEntitlement("compute", "compute.miloapis.com", metav1.ConditionTrue, "Complete"),
		*setupEntitlement("networking", "networking.miloapis.com", metav1.ConditionTrue, "Complete"),
		*setupEntitlement("ipam", "ipam.miloapis.com", metav1.ConditionTrue, "Complete"),
	}
	rootObjects := make([]client.Object, 0, len(services))
	for _, service := range services {
		rootObjects = append(rootObjects, service)
	}
	consumerObjects := make([]client.Object, 0, len(entitlements))
	stages := make(map[string]setupProjectionStage, len(entitlements))
	for i := range entitlements {
		consumerObjects = append(consumerObjects, entitlements[i].DeepCopy())
		stages[entitlements[i].Spec.ServiceRef.Name] = setupProjectionStage{complete: true, reason: "ProjectionConverged"}
	}

	r := &LocationProjectionReconciler{rootClient: projectionRootClient(rootObjects...)}
	consumer := projectionConsumerClient(consumerObjects...)
	if err := r.aggregateSetup(context.Background(), projectionProject, consumer, entitlements, stages); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ipam", "networking", "compute"} {
		var got servicesv1alpha1.ServiceEntitlement
		if err := consumer.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
			t.Fatal(err)
		}
		if !apimeta.IsStatusConditionTrue(got.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete) {
			t.Errorf("%s SetupComplete = %#v, want True", name,
				apimeta.FindStatusCondition(got.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete))
		}
	}
}

func TestTransitiveDependenciesCompleteReportsBlockingState(t *testing.T) {
	services := map[string]*servicesv1alpha1.Service{}
	for _, service := range []*servicesv1alpha1.Service{
		setupService("compute", "compute.miloapis.com", "networking"),
		setupService("networking", "networking.miloapis.com", "ipam"),
		setupService("ipam", "ipam.miloapis.com"),
	} {
		services[service.Name] = service
		services[service.Spec.ServiceName] = service
	}

	tests := []struct {
		name       string
		leafStatus metav1.ConditionStatus
		leafReason string
		removeLeaf bool
		wantReason string
	}{
		{name: "false dependency", leafStatus: metav1.ConditionFalse, leafReason: "EnrollmentFailed", wantReason: "EnrollmentFailed"},
		{name: "unknown dependency", leafStatus: metav1.ConditionUnknown, leafReason: "DiscoveryPending", wantReason: "DiscoveryPending"},
		{name: "missing dependency", leafStatus: metav1.ConditionTrue, leafReason: "Complete", removeLeaf: true, wantReason: "DependencyEntitlementMissing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compute := setupEntitlement("compute", "compute.miloapis.com", metav1.ConditionTrue, "Complete")
			networking := setupEntitlement("networking", "networking.miloapis.com", metav1.ConditionTrue, "Complete")
			ipam := setupEntitlement("ipam", "ipam.miloapis.com", tt.leafStatus, tt.leafReason)
			entitlements := map[string]*servicesv1alpha1.ServiceEntitlement{
				"compute": compute, "compute.miloapis.com": compute,
				"networking": networking, "networking.miloapis.com": networking,
				"ipam": ipam, "ipam.miloapis.com": ipam,
			}
			if tt.removeLeaf {
				delete(entitlements, "ipam")
				delete(entitlements, "ipam.miloapis.com")
			}
			complete, reason := transitiveDependenciesComplete(compute, services, entitlements, map[string]bool{})
			if complete || reason != tt.wantReason {
				t.Fatalf("complete, reason = %t, %q; want false, %q", complete, reason, tt.wantReason)
			}
		})
	}

	services["ipam"].Spec.Dependencies = []servicesv1alpha1.ServiceDependency{{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}}}
	compute := setupEntitlement("compute", "compute.miloapis.com", metav1.ConditionTrue, "Complete")
	networking := setupEntitlement("networking", "networking.miloapis.com", metav1.ConditionTrue, "Complete")
	ipam := setupEntitlement("ipam", "ipam.miloapis.com", metav1.ConditionTrue, "Complete")
	entitlements := map[string]*servicesv1alpha1.ServiceEntitlement{"compute": compute, "networking": networking, "ipam": ipam}
	if complete, reason := transitiveDependenciesComplete(compute, services, entitlements, map[string]bool{}); complete || reason != "DependencyCycle" {
		t.Fatalf("cycle complete, reason = %t, %q; want false, DependencyCycle", complete, reason)
	}
}

func setupService(name, canonical string, dependencies ...string) *servicesv1alpha1.Service {
	service := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       servicesv1alpha1.ServiceSpec{ServiceName: canonical},
	}
	for _, dependency := range dependencies {
		service.Spec.Dependencies = append(service.Spec.Dependencies, servicesv1alpha1.ServiceDependency{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: dependency},
		})
	}
	return service
}

func setupEntitlement(name, canonical string, dependencyStatus metav1.ConditionStatus, dependencyReason string) *servicesv1alpha1.ServiceEntitlement {
	entitlement := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: name}},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			Phase:       servicesv1alpha1.EntitlementPhaseActive,
			ServiceName: canonical,
		},
	}
	for _, condition := range []metav1.Condition{
		{Type: servicesv1alpha1.ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "Complete"},
		{Type: servicesv1alpha1.ConditionTypeDependenciesSatisfied, Status: dependencyStatus, Reason: dependencyReason},
		{Type: servicesv1alpha1.ConditionTypeProvisioned, Status: metav1.ConditionTrue, Reason: "Complete"},
	} {
		apimeta.SetStatusCondition(&entitlement.Status.Conditions, condition)
	}
	return entitlement
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
	r := &LocationProjectionReconciler{rootClient: projectionRootClient(), Manager: newTestManager(), projects: index}
	result, err := r.Reconcile(context.Background(), mcreconcile.Request{ClusterName: projectionProject})
	if err != nil || result != (ctrl.Result{}) {
		t.Fatalf("reconcile result=%+v err=%v", result, err)
	}
	if got := index.Projects(); len(got) != 0 {
		t.Fatalf("stale projects = %#v, want none", got)
	}
}

func TestLocationProjectionReconcilerDropsUnavailableProjectBeforeScanning(t *testing.T) {
	for _, tc := range []struct {
		name string
		root client.Client
	}{
		{name: "missing", root: projectionRootClientWithoutProject()},
		{name: "terminating", root: func() client.Client {
			now := metav1.Now()
			return projectionRootClient(&resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{
				Name: projectionProject, DeletionTimestamp: &now, Finalizers: []string{"test"},
			}})
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := newProjectionProjectIndex()
			index.Replace(projectionProject, []string{projectionService})
			stale := platformLocation(projectionLocation, true, projectionClass)
			stale.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
			consumer := projectionConsumerClient(stale)
			mgr := newTestManager()
			mgr.add(projectionProject, consumer)
			r := &LocationProjectionReconciler{rootClient: tc.root, Manager: mgr, projects: index}

			result, err := r.Reconcile(context.Background(), mcreconcile.Request{ClusterName: projectionProject})
			if err != nil || result != (ctrl.Result{}) {
				t.Fatalf("reconcile result=%+v err=%v", result, err)
			}
			if got := index.Projects(); len(got) != 0 {
				t.Fatalf("stale projects = %#v, want none", got)
			}
			if _, ok := projectedLocation(t, consumer, projectionLocation); !ok {
				t.Fatal("unavailable project was scanned and its existing projection was mutated")
			}
		})
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
