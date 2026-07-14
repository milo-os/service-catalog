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
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
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
		ClusterName: multicluster.ClusterName(cluster),
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
	if sc.Spec.ServiceRef.Name != testServiceSlug {
		t.Errorf("consumer.spec.serviceRef.name = %q, want entitlement ref %q (reconciler must mirror the ref, not canonicalize the immutable field)", sc.Spec.ServiceRef.Name, testServiceSlug)
	}
	if sc.Status.ServiceName != testServiceName {
		t.Errorf("consumer.status.serviceName = %q, want canonical %q", sc.Status.ServiceName, testServiceName)
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

	// Pin the bounded requeue for pending approvals (see
	// pendingApprovalRequeueInterval).
	res, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug))
	if err != nil {
		t.Fatalf("reconcile pending entitlement: %v", err)
	}
	if res != (ctrl.Result{RequeueAfter: pendingApprovalRequeueInterval}) {
		t.Errorf("pending reconcile result = %+v, want RequeueAfter=%v", res, pendingApprovalRequeueInterval)
	}

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
	if sc.Spec.ServiceRef.Name != testServiceSlug {
		t.Errorf("consumer.spec.serviceRef.name = %q, want entitlement ref %q (reconciler must mirror the ref, not canonicalize the immutable field)", sc.Spec.ServiceRef.Name, testServiceSlug)
	}
	if sc.Status.ServiceName != testServiceName {
		t.Errorf("consumer.status.serviceName = %q, want canonical %q", sc.Status.ServiceName, testServiceName)
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
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceName},
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

// terminatingEntitlement builds a ServiceEntitlement that is mid-finalize:
// it carries the finalizer and a deletion timestamp.
func terminatingEntitlement(name, serviceRef string) *servicesv1alpha1.ServiceEntitlement {
	now := metav1.NewTime(time.Now())
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Finalizers:        []string{serviceEntitlementFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: serviceRef},
		},
	}
}

// Two entitlements reference the same service. Finalizing one must NOT delete
// the shared ServiceConsumer while the other (active) entitlement remains.
func TestServiceEntitlementReconciler_DeleteKeepsConsumerWhenSiblingActive(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	finalizing := terminatingEntitlement("ent-a", testServiceSlug)
	sibling := newEntitlement("ent-b", testServiceSlug)

	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	existingConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceSlug},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
		},
	}

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(finalizing, sibling)
	providerClient := newFakeClient(existingConsumer)

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, "ent-a")); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
		t.Errorf("expected ServiceConsumer to survive while an active sibling entitlement remains, got err=%v", err)
	}
}

// Finalizing the LAST entitlement for a service deletes the shared
// ServiceConsumer even when a sibling for the same service is itself
// terminating — a concurrent/terminating sibling must not leak the consumer.
func TestServiceEntitlementReconciler_DeleteRemovesConsumerWhenSiblingTerminating(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	finalizing := terminatingEntitlement("ent-a", testServiceSlug)
	terminatingSibling := terminatingEntitlement("ent-b", testServiceSlug)

	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	existingConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceSlug},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
		},
	}

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(finalizing, terminatingSibling)
	providerClient := newFakeClient(existingConsumer)

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, "ent-a")); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected ServiceConsumer to be deleted when all remaining siblings are terminating, got err=%v", err)
	}
}

// A sibling entitlement that references a DIFFERENT service does not keep the
// consumer alive — its different serviceRef maps to a different ServiceConsumer.
func TestServiceEntitlementReconciler_DeleteIgnoresSiblingForDifferentService(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	finalizing := terminatingEntitlement("ent-a", testServiceSlug)
	otherService := newEntitlement("ent-other", testDepServiceSlug)

	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	existingConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceSlug},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
		},
	}

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(finalizing, otherService)
	providerClient := newFakeClient(existingConsumer)

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, "ent-a")); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected ServiceConsumer to be deleted; a sibling for a different service must not keep it alive, got err=%v", err)
	}
}

func TestServiceEntitlementReconciler_StampsCanonicalServiceNameInStatus(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	// Entitlement created with the k8s object name — spec is left as-is but
	// status.serviceName should be stamped with the canonical identifier.
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

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 10)

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.ServiceName != testServiceName {
		t.Errorf("status.serviceName = %q, want canonical %q", got.Status.ServiceName, testServiceName)
	}
	// spec.serviceRef.name must NOT be mutated by the reconciler.
	if got.Spec.ServiceRef.Name != testServiceSlug {
		t.Errorf("spec.serviceRef.name = %q, want original %q (reconciler must not mutate spec)", got.Spec.ServiceRef.Name, testServiceSlug)
	}
}

// TestServiceEntitlementReconciler_DoesNotRewriteExistingConsumerSpec verifies
// the reconciler leaves the identity fields of a pre-existing ServiceConsumer
// alone. spec.serviceRef and spec.consumerProjectRef are immutable after
// creation (enforced by the ServiceConsumer webhook for everyone, including
// the controller), so rewriting them on a consumer whose stored ref differs
// would produce an Update the webhook rejects, wedging the reconcile.
func TestServiceEntitlementReconciler_DoesNotRewriteExistingConsumerSpec(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	// Pre-existing consumer (old vintage): the canonical name was written
	// into the immutable spec.serviceRef directly. CreationTimestamp is set
	// explicitly because the fake client does not stamp it on seeded objects.
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	existingConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{
			Name:              consumerName,
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceName},
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

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var sc servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &sc); err != nil {
		t.Fatalf("get serviceconsumer: %v", err)
	}
	if sc.Spec.ServiceRef.Name != testServiceName {
		t.Errorf("consumer.spec.serviceRef.name = %q, want untouched %q (identity fields are immutable; the reconciler must not rewrite them on existing consumers)", sc.Spec.ServiceRef.Name, testServiceName)
	}
	if sc.Spec.ConsumerProjectRef.Name != testConsumerProject {
		t.Errorf("consumer.spec.consumerProjectRef.name = %q, want untouched %q", sc.Spec.ConsumerProjectRef.Name, testConsumerProject)
	}
	// Status reconciliation must still proceed for the existing consumer.
	if sc.Status.ServiceName != testServiceName {
		t.Errorf("consumer.status.serviceName = %q, want canonical %q", sc.Status.ServiceName, testServiceName)
	}
	if sc.Status.Phase != servicesv1alpha1.ConsumerPhaseActive {
		t.Errorf("consumer phase = %q, want Active", sc.Status.Phase)
	}
}

// TestResolveService_CanonicalIndexHit resolves a Service when the lookup name
// equals spec.serviceName, exercising the canonical-index hit path. The Service
// object name differs from its canonical name so a name-based Get would miss.
func TestResolveService_CanonicalIndexHit(t *testing.T) {
	svc := newPublishedService("compute-obj-name", testServiceName, testProviderProject, "")

	r := &ServiceEntitlementReconciler{
		rootClient: newFakeClient(svc),
		Manager:    newTestManager(),
		Scheme:     testScheme(),
	}

	// Look up by canonical name; the index must satisfy it without falling
	// back to a Get on the object name.
	got, err := r.resolveService(context.Background(), testServiceName)
	if err != nil {
		t.Fatalf("resolveService by canonical name: %v", err)
	}
	if got.Name != "compute-obj-name" {
		t.Errorf("resolved object name = %q, want %q", got.Name, "compute-obj-name")
	}
	if got.Spec.ServiceName != testServiceName {
		t.Errorf("resolved canonical name = %q, want %q", got.Spec.ServiceName, testServiceName)
	}
}

// TestResolveService_ObjectNameFallback resolves a Service when the lookup name
// matches ONLY the Service object name (not spec.serviceName), exercising the
// backward-compatible name-based Get fallback after the canonical index misses.
func TestResolveService_ObjectNameFallback(t *testing.T) {
	// Canonical name differs from the object name we will look up by.
	svc := newPublishedService("compute-obj-name", testServiceName, testProviderProject, "")

	r := &ServiceEntitlementReconciler{
		rootClient: newFakeClient(svc),
		Manager:    newTestManager(),
		Scheme:     testScheme(),
	}

	// "compute-obj-name" is not any Service's spec.serviceName, so the index
	// lookup misses and resolveService must fall back to a Get by object name.
	got, err := r.resolveService(context.Background(), "compute-obj-name")
	if err != nil {
		t.Fatalf("resolveService by object name (fallback): %v", err)
	}
	if got.Name != "compute-obj-name" {
		t.Errorf("resolved object name = %q, want %q", got.Name, "compute-obj-name")
	}
}

// TestEnsureDependencies_NoDuplicateByCanonicalName verifies the dedup guard:
// when an entitlement for the dependency's canonical service name already
// exists under a DIFFERENT metadata.name, ensureDependencies must not create a
// second (duplicate) dependency entitlement.
func TestEnsureDependencies_NoDuplicateByCanonicalName(t *testing.T) {
	const depCanonical = "storage.miloapis.com"

	parentSvc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", testDepServiceSlug)
	depSvc := newPublishedService(testDepServiceSlug, depCanonical, testProviderProject, "")
	parentEnt := newEntitlement(testServiceSlug, testServiceSlug)

	// Pre-seed an entitlement for the same dependency service under a
	// different metadata.name, with status.serviceName == the canonical name.
	preExisting := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "preexisting-storage"},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: depCanonical},
		},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			ServiceName: depCanonical,
		},
	}

	rootClient := newFakeClient(parentSvc, depSvc)
	consumerClient := newFakeClient(parentEnt, preExisting)
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

	// No dependency entitlement should have been created under the dependency
	// object name (testDepServiceSlug), since one already exists by canonical
	// name under a different metadata.name.
	var dup servicesv1alpha1.ServiceEntitlement
	err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testDepServiceSlug}, &dup)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected NO duplicate dependency entitlement %q, got err=%v", testDepServiceSlug, err)
	}

	// Exactly one entitlement for the dependency canonical name must exist.
	var all servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(context.Background(), &all); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	count := 0
	for i := range all.Items {
		if all.Items[i].Status.ServiceName == depCanonical {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d entitlements for dependency %q, want 1", count, depCanonical)
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
