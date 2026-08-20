// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
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
	// Admission-emulating client: the dependency Service's object name
	// ("storage") differs from its canonical name ("storage.miloapis.com"),
	// which is the shape that made the derived entitlement inadmissible.
	consumerClient := newAdmissionFakeClient(rootClient, parentEnt)
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
	if depEnt.Spec.ServiceRef.Name != testDepServiceSlug {
		t.Errorf("dependency entitlement serviceRef.name = %q, want object name %q (admission resolves by metadata.name)",
			depEnt.Spec.ServiceRef.Name, testDepServiceSlug)
	}
	if depEnt.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
		t.Errorf("dependency entitlement origin = %q, want Dependency", depEnt.Status.Origin)
	}
	if depEnt.Status.DependencyOf != testServiceSlug {
		t.Errorf("dependency entitlement dependencyOf = %q, want %q", depEnt.Status.DependencyOf, testServiceSlug)
	}

	var parent servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &parent); err != nil {
		t.Fatalf("get parent entitlement: %v", err)
	}
	if cond := apimeta.FindStatusCondition(parent.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("parent DependenciesSatisfied = %v, want True", cond)
	}
}

// TestServiceEntitlementReconciler_DependencyUnpublished covers the reporting
// gap: an entitlement whose dependency can't be enrolled must say so rather
// than looking fully enabled. Ready stays True — this entitlement's own access
// really was granted.
func TestServiceEntitlementReconciler_DependencyUnpublished(t *testing.T) {
	parentSvc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", testDepServiceSlug)
	depSvc := newPublishedService(testDepServiceSlug, "storage.miloapis.com", testProviderProject, "")
	depSvc.Spec.Phase = servicesv1alpha1.PhaseDraft
	parentEnt := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(parentSvc, depSvc)
	// Draft dependency: admission refuses to enable an unpublished service.
	consumerClient := &admissionClient{Client: newFakeClient(parentEnt), root: newFakeClient(parentSvc)}
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	// Errors are expected here, so drive the reconcile directly.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug))
	}

	var parent servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &parent); err != nil {
		t.Fatalf("get parent entitlement: %v", err)
	}
	cond := apimeta.FindStatusCondition(parent.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("parent DependenciesSatisfied = %v, want False", cond)
	}
	if !strings.Contains(cond.Message, testDepServiceSlug) {
		t.Errorf("condition message %q doesn't name the failing dependency %q", cond.Message, testDepServiceSlug)
	}
	if ready := apimeta.FindStatusCondition(parent.Status.Conditions, ConditionTypeReady); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %v, want True (a dependency failure is not a denial)", ready)
	}
}

// TestMapServiceToServiceEntitlements verifies the fan-out that makes
// enrollment reach projects that enabled the service before the dependency was
// declared: a Service change must enqueue every entitlement naming it.
func TestMapServiceToServiceEntitlements(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")

	entitled := newEntitlement(testServiceSlug, testServiceSlug)
	entitled.Status.ServiceName = testServiceName
	other := newEntitlement("other", "other")
	other.Status.ServiceName = "other.miloapis.com"

	rootClient := newFakeClient(
		svc,
		&resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: testConsumerProject}},
		&resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "empty-project"}},
	)

	mgr := newTestManager()
	mgr.add(testConsumerProject, newFakeClient(entitled, other))
	mgr.add("empty-project", newFakeClient())

	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	reqs := r.mapServiceToServiceEntitlements(context.Background(), svc)
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Name != testServiceSlug || string(reqs[0].ClusterName) != testConsumerProject {
		t.Errorf("request = %s/%s, want %s/%s", reqs[0].ClusterName, reqs[0].Name, testConsumerProject, testServiceSlug)
	}
}

// TestServiceEnrollmentPredicate keeps the fan-out off the hot path: a Service
// status write shouldn't re-reconcile every entitlement in every project.
func TestServiceEnrollmentPredicate(t *testing.T) {
	p := serviceEnrollmentPredicate()
	base := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")

	statusOnly := base.DeepCopy()
	statusOnly.Status.ObservedGeneration = 7
	if p.Update(event.TypedUpdateEvent[*servicesv1alpha1.Service]{ObjectOld: base, ObjectNew: statusOnly}) {
		t.Error("status-only update should not trigger the fan-out")
	}

	cosmetic := base.DeepCopy()
	cosmetic.Spec.DisplayName = "Renamed"
	if p.Update(event.TypedUpdateEvent[*servicesv1alpha1.Service]{ObjectOld: base, ObjectNew: cosmetic}) {
		t.Error("displayName update should not trigger the fan-out")
	}

	withDep := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", testDepServiceSlug)
	if !p.Update(event.TypedUpdateEvent[*servicesv1alpha1.Service]{ObjectOld: base, ObjectNew: withDep}) {
		t.Error("adding a dependency must trigger the fan-out")
	}

	deprecated := base.DeepCopy()
	deprecated.Spec.Phase = servicesv1alpha1.PhaseDeprecated
	if !p.Update(event.TypedUpdateEvent[*servicesv1alpha1.Service]{ObjectOld: base, ObjectNew: deprecated}) {
		t.Error("a phase change must trigger the fan-out")
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

// providerTeardownFinalizerString mirrors the deprovisioning finalizer the
// consumer-project provider SDK stamps on a ServiceConsumer. It is duplicated
// here (the SDK constant is unexported and in another package) so the handshake
// test can simulate a provider that gates its ServiceConsumer's deletion.
const providerTeardownFinalizerString = "services.miloapis.com/provider-teardown"

// When the last entitlement for a service is finalized but the provider is
// still tearing down (its finalizer keeps the ServiceConsumer alive), the
// entitlement's own finalizer must be HELD — otherwise the project could
// complete deletion while provider resources still exist. The finalize must
// requeue rather than remove the finalizer.
func TestServiceEntitlementReconciler_DeleteWaitsForProviderTeardown(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := terminatingEntitlement(testServiceSlug, testServiceSlug)

	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	// The provider's deprovisioning finalizer gates the consumer's deletion.
	gatedConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{
			Name:       consumerName,
			Finalizers: []string{providerTeardownFinalizerString},
		},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceName},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
		},
	}

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(gatedConsumer)

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	res, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug))
	if err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if res.RequeueAfter != providerTeardownRequeueInterval {
		t.Errorf("expected requeue while provider teardown is pending, got RequeueAfter=%v", res.RequeueAfter)
	}

	// The entitlement finalizer must still be present — deletion is blocked.
	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("entitlement should still exist while gated: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&gotEnt, serviceEntitlementFinalizer) {
		t.Errorf("entitlement finalizer must be held until provider confirms teardown")
	}

	// The ServiceConsumer was asked to delete but is held by the provider finalizer.
	var gotSC servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &gotSC); err != nil {
		t.Fatalf("gated ServiceConsumer should still exist: %v", err)
	}
	if gotSC.DeletionTimestamp.IsZero() {
		t.Errorf("ServiceConsumer should be marked for deletion (deletionTimestamp set)")
	}
}

// Once the provider confirms teardown (drops its finalizer, the ServiceConsumer
// is garbage-collected), the next finalize pass removes the entitlement
// finalizer so the entitlement — and the project — can complete deletion.
func TestServiceEntitlementReconciler_DeleteCompletesAfterProviderTeardown(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := terminatingEntitlement(testServiceSlug, testServiceSlug)

	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	gatedConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{
			Name:       consumerName,
			Finalizers: []string{providerTeardownFinalizerString},
		},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceName},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
		},
	}

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(gatedConsumer)

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	// Pass 1: gated — finalizer held.
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("reconcile (gated): %v", err)
	}
	var stillThere servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &stillThere); err != nil {
		t.Fatalf("entitlement should still exist while gated: %v", err)
	}

	// Provider confirms teardown: drop its finalizer so the ServiceConsumer is
	// garbage-collected.
	var sc servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &sc); err != nil {
		t.Fatalf("get gated consumer: %v", err)
	}
	controllerutil.RemoveFinalizer(&sc, providerTeardownFinalizerString)
	if err := providerClient.Update(context.Background(), &sc); err != nil {
		t.Fatalf("provider drop finalizer: %v", err)
	}
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &sc); !apierrors.IsNotFound(err) {
		t.Fatalf("consumer should be gone after provider drops its finalizer, err=%v", err)
	}

	// Pass 2: teardown confirmed — entitlement finalizer removed, deletion completes.
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("reconcile (post-teardown): %v", err)
	}
	var gone servicesv1alpha1.ServiceEntitlement
	err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("entitlement should be finalized once teardown is confirmed, err=%v", err)
	}
}

// TestEnsureDependencies_LabelsDependencyForProvenance pins the record the
// origin stamp is derived from. Without it the stamp is a status write on a
// just-created object, racing that object's own first reconcile.
func TestEnsureDependencies_LabelsDependencyForProvenance(t *testing.T) {
	parentSvc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", testDepServiceSlug)
	depSvc := newPublishedService(testDepServiceSlug, "storage.miloapis.com", testProviderProject, "")
	parentEnt := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(parentSvc, depSvc)
	consumerClient := newAdmissionFakeClient(rootClient, parentEnt)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var depEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testDepServiceSlug}, &depEnt); err != nil {
		t.Fatalf("dependency entitlement not created: %v", err)
	}
	if got := depEnt.Labels[dependencyOfLabel]; got != testServiceSlug {
		t.Errorf("dependency label = %q, want %q", got, testServiceSlug)
	}
}

// TestDependencyOriginSurvivesItsOwnReconcile is the regression for the race
// that left enrolled dependencies marked Direct: the dependency entitlement's
// own reconcile used to default origin on an empty value, clobbering the
// parent's stamp. Origin must be derived from the label instead, so a status
// write that lands after the stamp restores it rather than reverting it.
func TestDependencyOriginSurvivesItsOwnReconcile(t *testing.T) {
	depSvc := newPublishedService(testDepServiceSlug, "storage.miloapis.com", testProviderProject, "")

	// A dependency entitlement as it exists mid-race: labelled by the parent,
	// but status not yet stamped.
	depEnt := newEntitlement(testDepServiceSlug, testDepServiceSlug)
	depEnt.Labels = map[string]string{dependencyOfLabel: testServiceSlug}

	rootClient := newFakeClient(depSvc)
	consumerClient := newFakeClient(depEnt)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testDepServiceSlug), 5)

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testDepServiceSlug}, &got); err != nil {
		t.Fatalf("get dependency entitlement: %v", err)
	}
	if got.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
		t.Errorf("origin = %q, want Dependency", got.Status.Origin)
	}
	if got.Status.DependencyOf != testServiceSlug {
		t.Errorf("dependencyOf = %q, want %q", got.Status.DependencyOf, testServiceSlug)
	}
}

// TestUnlabelledEntitlementStaysDirect is the other half: an entitlement the
// consumer created themselves must not be adopted as a dependency, which would
// hand it delete protection they never asked for.
func TestUnlabelledEntitlementStaysDirect(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.Origin != servicesv1alpha1.EntitlementOriginDirect {
		t.Errorf("origin = %q, want Direct", got.Status.Origin)
	}
	if got.Status.DependencyOf != "" {
		t.Errorf("dependencyOf = %q, want empty", got.Status.DependencyOf)
	}
}

// TestServiceEntitlementReconciler_TransitiveDependencyEntitlementCreated
// covers a three-service chain, which is the normal shape here: a product
// capability depends on a networking capability, which depends on the
// addressing capability underneath it. Enabling the first must end with all
// three enabled, not just the first two.
func TestServiceEntitlementReconciler_TransitiveDependencyEntitlementCreated(t *testing.T) {
	const (
		midSlug  = "networking"
		leafSlug = "ipam"
	)
	topSvc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", midSlug)
	midSvc := newPublishedService(midSlug, "networking.miloapis.com", testProviderProject, "", leafSlug)
	leafSvc := newPublishedService(leafSlug, "ipam.miloapis.com", testProviderProject, "")
	topEnt := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(topSvc, midSvc, leafSvc)
	consumerClient := newAdmissionFakeClient(rootClient, topEnt)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	// Each entitlement reconciles on its own; the create of the middle
	// entitlement is what wakes it in the real controller.
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, midSlug), 5)
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, leafSlug), 5)

	var midEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: midSlug}, &midEnt); err != nil {
		t.Fatalf("first-hop dependency entitlement not created: %v", err)
	}

	var leafEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: leafSlug}, &leafEnt); err != nil {
		t.Fatalf("transitive dependency entitlement not created: %v", err)
	}
	if leafEnt.Spec.ServiceRef.Name != leafSlug {
		t.Errorf("transitive entitlement serviceRef.name = %q, want object name %q", leafEnt.Spec.ServiceRef.Name, leafSlug)
	}
	if leafEnt.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
		t.Errorf("transitive entitlement origin = %q, want Dependency", leafEnt.Status.Origin)
	}
	if leafEnt.Status.DependencyOf != midSlug {
		t.Errorf("transitive entitlement dependencyOf = %q, want %q", leafEnt.Status.DependencyOf, midSlug)
	}
	if leafEnt.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("transitive entitlement phase = %q, want Active", leafEnt.Status.Phase)
	}

	if cond := apimeta.FindStatusCondition(midEnt.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("middle DependenciesSatisfied = %v, want True", cond)
	}
}

// TestServiceEntitlementReconciler_DependencyCycleConverges proves that
// enrolling dependencies of dependencies stays safe when two services declare
// each other. Enrollment is level-triggered and entitlements are named for the
// dependency Service, so a cycle settles on one entitlement per service and
// then stops writing.
func TestServiceEntitlementReconciler_DependencyCycleConverges(t *testing.T) {
	const otherSlug = testDepServiceSlug
	svcA := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", otherSlug)
	svcB := newPublishedService(otherSlug, "storage.miloapis.com", testProviderProject, "", testServiceSlug)
	entA := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svcA, svcB)
	consumerClient := newAdmissionFakeClient(rootClient, entA)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	for i := 0; i < 5; i++ {
		reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)
		reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, otherSlug), 5)
	}

	var all servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(context.Background(), &all); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	if len(all.Items) != 2 {
		names := make([]string, 0, len(all.Items))
		for i := range all.Items {
			names = append(names, all.Items[i].Name)
		}
		t.Fatalf("entitlement count = %d (%v), want 2 (one per service in the cycle)", len(all.Items), names)
	}

	// A converged cycle must stop writing: another full round should leave
	// every object's resourceVersion untouched.
	before := map[string]string{}
	for i := range all.Items {
		before[all.Items[i].Name] = all.Items[i].ResourceVersion
	}
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, otherSlug), 5)

	var after servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(context.Background(), &after); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	for i := range after.Items {
		item := &after.Items[i]
		if before[item.Name] != item.ResourceVersion {
			t.Errorf("entitlement %q was rewritten on a settled reconcile (resourceVersion %s -> %s); the cycle is not converging",
				item.Name, before[item.Name], item.ResourceVersion)
		}
	}

	// The entitlement the project asked for directly keeps its provenance: a
	// cycle must not let the derived side claim it as a dependency.
	var direct servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &direct); err != nil {
		t.Fatalf("get direct entitlement: %v", err)
	}
	if direct.Status.Origin != servicesv1alpha1.EntitlementOriginDirect {
		t.Errorf("direct entitlement origin = %q, want Direct", direct.Status.Origin)
	}
}
