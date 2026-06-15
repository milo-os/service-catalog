// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newServiceConsumer(name string, approval *servicesv1alpha1.ProviderApproval) *servicesv1alpha1.ServiceConsumer {
	return &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: testServiceSlug},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
			Approval:           approval,
		},
	}
}

func TestServiceConsumerReconciler_Approved(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	// Stamped shape: the entitlement reconciler has put the canonical name on
	// status.serviceName of both objects.
	sc.Status.ServiceName = testServiceName
	// Entitlement lives in the consumer cluster, keyed by service slug.
	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Status.ServiceName = testServiceName

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotSC servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &gotSC); err != nil {
		t.Fatalf("get sc: %v", err)
	}
	if gotSC.Status.Phase != servicesv1alpha1.ConsumerPhaseActive {
		t.Errorf("consumer phase = %q, want Active", gotSC.Status.Phase)
	}

	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active", gotEnt.Status.Phase)
	}
}

func TestServiceConsumerReconciler_Denied(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionDenied,
	})
	sc.Status.ServiceName = testServiceName
	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Status.ServiceName = testServiceName

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{Manager: mgr, Scheme: testScheme()}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotSC servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &gotSC); err != nil {
		t.Fatalf("get sc: %v", err)
	}
	if gotSC.Status.Phase != servicesv1alpha1.ConsumerPhaseDenied {
		t.Errorf("consumer phase = %q, want Denied", gotSC.Status.Phase)
	}

	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != servicesv1alpha1.EntitlementPhaseRejected {
		t.Errorf("entitlement phase = %q, want Rejected", gotEnt.Status.Phase)
	}
}

// TestServiceConsumerReconciler_ApprovedByServiceRefNotMetadataName verifies
// that approval propagates to an entitlement whose metadata.name differs from
// the consumer's spec.serviceRef.name. This is the common case in production
// where a user names their entitlement something short (e.g. "my-compute")
// while the service slug stored on the consumer is "compute".
func TestServiceConsumerReconciler_ApprovedByServiceRefNotMetadataName(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	sc.Status.ServiceName = testServiceName
	// Entitlement has a user-chosen metadata.name that differs from the
	// serviceRef name stored on the ServiceConsumer.
	const userChosenName = "my-compute"
	ent := newEntitlement(userChosenName, testServiceSlug)
	ent.Status.ServiceName = testServiceName

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Consumer status should be Active.
	var gotSC servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &gotSC); err != nil {
		t.Fatalf("get sc: %v", err)
	}
	if gotSC.Status.Phase != servicesv1alpha1.ConsumerPhaseActive {
		t.Errorf("consumer phase = %q, want Active", gotSC.Status.Phase)
	}

	// Entitlement status should be Active — looked up by canonical service
	// name, not by metadata.name.
	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: userChosenName}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active", gotEnt.Status.Phase)
	}
}

// TestServiceConsumerReconciler_ApprovedCanonicalServiceName reproduces issue
// #42 with the production object shapes — the caller's verbatim ref (e.g.
// "compute") in spec.serviceRef of both objects, the canonical name (e.g.
// "compute.miloapis.com") stamped on status.serviceName of both — and
// asserts approval activates the entitlement.
func TestServiceConsumerReconciler_ApprovedCanonicalServiceName(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	sc.Status.ServiceName = testServiceName

	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Status.ServiceName = testServiceName

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotSC servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &gotSC); err != nil {
		t.Fatalf("get sc: %v", err)
	}
	if gotSC.Status.Phase != servicesv1alpha1.ConsumerPhaseActive {
		t.Errorf("consumer phase = %q, want Active", gotSC.Status.Phase)
	}

	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active", gotEnt.Status.Phase)
	}
}

// TestServiceConsumerReconciler_ApprovedMixedVintageConsumer covers consumers
// created before the verbatim-ref convention, whose spec.serviceRef.name
// already holds the canonical service name. With the canonical name also
// stamped on status.serviceName of both sides, approval must still find the
// entitlement.
func TestServiceConsumerReconciler_ApprovedMixedVintageConsumer(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	// Old vintage: the canonical name was written into the immutable
	// spec.serviceRef directly.
	sc.Spec.ServiceRef.Name = testServiceName
	sc.Status.ServiceName = testServiceName

	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Status.ServiceName = testServiceName

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active", gotEnt.Status.Phase)
	}
}

// TestServiceConsumerReconciler_SkipsUntilConsumerStamped verifies the
// skip-until-stamped guard: a decided consumer whose status.serviceName
// hasn't been stamped yet is skipped without error or requeue, even when an
// entitlement matching the verbatim spec ref exists. The entitlement
// reconciler's status stamp fires this controller's own watch, so the skip is
// re-triggered event-driven.
func TestServiceConsumerReconciler_SkipsUntilConsumerStamped(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	// status.serviceName deliberately left empty: not yet stamped.

	// An entitlement matching the consumer's verbatim slug ref exists, but
	// the reconciler must not act on the spec ref alone.
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	res, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("result = %+v, want zero result (re-trigger is event-driven via the status stamp)", res)
	}

	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != "" {
		t.Errorf("entitlement phase = %q, want unchanged (empty)", gotEnt.Status.Phase)
	}
}

// TestServiceConsumerReconciler_UnstampedEntitlementDoesNotMatch verifies
// that matching works exclusively off the stamped canonical name: an
// entitlement whose status.serviceName hasn't been stamped yet is invisible
// to the field index — even if its spec.serviceRef holds the canonical name —
// so the reconcile returns the no-match error (retried via backoff until the
// entitlement reconciler stamps it) and leaves the entitlement unmutated.
func TestServiceConsumerReconciler_UnstampedEntitlementDoesNotMatch(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	sc.Status.ServiceName = testServiceName

	// Canonical ref in spec, status not yet stamped.
	ent := newEntitlement(testServiceSlug, testServiceName)

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(ent)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err == nil {
		t.Fatalf("expected the no-match error for an unstamped entitlement, got nil")
	}

	var gotEnt servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &gotEnt); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if gotEnt.Status.Phase != "" {
		t.Errorf("entitlement phase = %q, want unchanged (empty)", gotEnt.Status.Phase)
	}
}

// TestServiceConsumerReconciler_ErrorWhenNoMatchingEntitlement verifies that
// a decided consumer with no matching entitlement in the consumer project is
// surfaced as a reconcile error. Consumers are only created by the
// entitlement reconciler, so this state is an inconsistency (or a
// self-resolving deletion race).
func TestServiceConsumerReconciler_ErrorWhenNoMatchingEntitlement(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	sc.Status.ServiceName = testServiceName

	// The consumer project has an entitlement for a different service only;
	// nothing matches the consumer's canonical service name.
	other := newEntitlement(testDepServiceSlug, testDepServiceSlug)
	other.Status.ServiceName = "storage.miloapis.com"

	providerClient := newFakeClient(sc)
	consumerClient := newFakeClient(other)

	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)

	r := &ServiceConsumerReconciler{
		Manager: mgr,
		Scheme:  testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err == nil {
		t.Fatalf("expected an error when no ServiceEntitlement matches the consumer, got nil")
	}
}

func TestServiceConsumerReconciler_NoApprovalIsNoOp(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, nil)

	providerClient := newFakeClient(sc)
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)

	r := &ServiceConsumerReconciler{Manager: mgr, Scheme: testScheme()}

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, consumerName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
		t.Fatalf("get sc: %v", err)
	}
	if got.Status.Phase != "" {
		t.Errorf("expected empty phase on no-op reconcile, got %q", got.Status.Phase)
	}
}
