// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const (
	testConsumerProject = "consumer-proj"
	testProviderProject = "provider-proj"
	testServiceName     = "compute.miloapis.com"
	testServiceSlug     = "compute"
	testDepServiceSlug  = "storage"
)

func newPublishedService(name, canonical, providerProject string, mode servicesv1alpha1.EnablementMode, deps ...string) *servicesv1alpha1.Service {
	svc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: canonical,
			DisplayName: name,
			Phase:       servicesv1alpha1.PhasePublished,
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: providerProject},
			},
		},
	}
	if mode != "" {
		svc.Spec.EnablementPolicy = &servicesv1alpha1.EnablementPolicy{Mode: mode}
	}
	for _, d := range deps {
		svc.Spec.Dependencies = append(svc.Spec.Dependencies, servicesv1alpha1.ServiceDependency{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: d},
		})
	}
	return svc
}

func newEntitlement(name, serviceRef string) *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: serviceRef},
		},
	}
}

func entitlementRequest(cluster, name string) mcreconcile.Request {
	return mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: name}},
		ClusterName: cluster,
	}
}

// reconcileUntilStable reconciles repeatedly until either no change is
// observed or maxIters is hit. This lets us drive the controller through
// its add-finalizer / set-status passes in a single test setup.
func reconcileUntilStable(t *testing.T, r *ServiceEntitlementReconciler, req mcreconcile.Request, maxIters int) {
	t.Helper()
	for i := 0; i < maxIters; i++ {
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile iter %d: %v", i, err)
		}
		if res.RequeueAfter == 0 {
			// run one more pass to be sure nothing changes
			res2, err2 := r.Reconcile(context.Background(), req)
			if err2 != nil {
				t.Fatalf("Reconcile stable check: %v", err2)
			}
			if res2.RequeueAfter == 0 {
				return
			}
		}
	}
}

func TestServiceEntitlementReconciler_SelfServiceActive(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	// Entitlement should be Active in consumer cluster.
	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active", got.Status.Phase)
	}
	if got.Status.EntitledAt == nil {
		t.Errorf("expected EntitledAt to be set on Active entitlement")
	}

	// ServiceConsumer should exist in provider cluster, phase Active.
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	var sc servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &sc); err != nil {
		t.Fatalf("get serviceconsumer: %v", err)
	}
	if sc.Spec.ConsumerProjectRef.Name != testConsumerProject {
		t.Errorf("consumer.spec.consumerProjectRef.name = %q, want %q", sc.Spec.ConsumerProjectRef.Name, testConsumerProject)
	}
	if sc.Status.Phase != servicesv1alpha1.ConsumerPhaseActive {
		t.Errorf("consumer phase = %q, want Active", sc.Status.Phase)
	}
}

func TestServiceEntitlementReconciler_GatedPendingApproval(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, servicesv1alpha1.EnablementModeGatedByProvider)
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhasePendingApproval {
		t.Errorf("entitlement phase = %q, want PendingApproval", got.Status.Phase)
	}

	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	var sc servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &sc); err != nil {
		t.Fatalf("get serviceconsumer: %v", err)
	}
	if sc.Status.Phase != servicesv1alpha1.ConsumerPhasePendingApproval {
		t.Errorf("consumer phase = %q, want PendingApproval", sc.Status.Phase)
	}
}

func TestServiceEntitlementReconciler_DependencyEntitlementCreated(t *testing.T) {
	parentSvc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", testDepServiceSlug)
	depSvc := newPublishedService(testDepServiceSlug, "storage.miloapis.com", testProviderProject, "")
	parentEnt := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(parentSvc, depSvc)
	consumerClient := newFakeClient(parentEnt)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var depEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testDepServiceSlug}, &depEnt); err != nil {
		t.Fatalf("dependency entitlement not created: %v", err)
	}
	if depEnt.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
		t.Errorf("dependency entitlement origin = %q, want Dependency", depEnt.Status.Origin)
	}
	if depEnt.Status.DependencyOf != testServiceSlug {
		t.Errorf("dependency entitlement dependencyOf = %q, want %q", depEnt.Status.DependencyOf, testServiceSlug)
	}
}

func TestServiceEntitlementReconciler_AddsFinalizer(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if !hasFinalizer(&got, serviceEntitlementFinalizer) {
		t.Errorf("expected finalizer %q to be added on first reconcile", serviceEntitlementFinalizer)
	}
}

func TestServiceEntitlementReconciler_DeleteRemovesConsumer(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	now := metav1.NewTime(time.Now())
	ent := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testServiceSlug,
			Finalizers:        []string{serviceEntitlementFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: testServiceSlug},
		},
	}
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	existingConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceSlug},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
		},
	}

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(existingConsumer)

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected ServiceConsumer to be deleted, got err=%v", err)
	}
}

func hasFinalizer(obj client.Object, f string) bool {
	for _, x := range obj.GetFinalizers() {
		if x == f {
			return true
		}
	}
	return false
}
