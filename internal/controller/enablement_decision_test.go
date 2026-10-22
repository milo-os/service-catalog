// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// enablementFixture wires both reconcilers against one consumer and one
// provider project so a test can drive the full request/decision round trip.
type enablementFixture struct {
	consumerClient client.Client
	providerClient client.Client
	entitlementR   *ServiceEntitlementReconciler
	consumerR      *ServiceConsumerReconciler
}

func newEnablementFixture(mode servicesv1alpha1.EnablementMode, ent *servicesv1alpha1.ServiceEntitlement) *enablementFixture {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, mode)
	f := &enablementFixture{
		consumerClient: newFakeClient(ent),
		providerClient: newFakeClient(),
	}
	mgr := newTestManager()
	mgr.add(testConsumerProject, f.consumerClient)
	mgr.add(testProviderProject, f.providerClient)
	f.entitlementR = &ServiceEntitlementReconciler{rootClient: newServiceRootClient(svc), Manager: mgr, Scheme: testScheme()}
	f.consumerR = &ServiceConsumerReconciler{Manager: mgr, Scheme: testScheme()}
	return f
}

func (f *enablementFixture) reconcileEntitlement(t *testing.T) {
	t.Helper()
	reconcileUntilStable(t, f.entitlementR, entitlementRequest(testConsumerProject, testServiceSlug), 5)
}

func (f *enablementFixture) decide(t *testing.T, approval servicesv1alpha1.ProviderApproval) {
	t.Helper()
	sc := f.consumer(t)
	sc.Spec.Approval = &approval
	if err := f.providerClient.Update(context.Background(), sc); err != nil {
		t.Fatalf("record decision: %v", err)
	}
	if _, err := f.consumerR.Reconcile(context.Background(), entitlementRequest(testProviderProject, sc.Name)); err != nil {
		t.Fatalf("reconcile consumer: %v", err)
	}
}

func (f *enablementFixture) consumer(t *testing.T) *servicesv1alpha1.ServiceConsumer {
	t.Helper()
	var sc servicesv1alpha1.ServiceConsumer
	name := serviceConsumerName(testServiceName, testConsumerProject)
	if err := f.providerClient.Get(context.Background(), types.NamespacedName{Name: name}, &sc); err != nil {
		t.Fatalf("get serviceconsumer: %v", err)
	}
	return &sc
}

func (f *enablementFixture) entitlementReady(t *testing.T) *metav1.Condition {
	t.Helper()
	var ent servicesv1alpha1.ServiceEntitlement
	if err := f.consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &ent); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	cond := apimeta.FindStatusCondition(ent.Status.Conditions, ConditionTypeReady)
	if cond == nil {
		t.Fatal("entitlement has no Ready condition")
	}
	return cond
}

func consumerReady(t *testing.T, sc *servicesv1alpha1.ServiceConsumer) *metav1.Condition {
	t.Helper()
	cond := apimeta.FindStatusCondition(sc.Status.Conditions, ConditionTypeReady)
	if cond == nil {
		t.Fatal("serviceconsumer has no Ready condition")
	}
	return cond
}

func TestEnablement_SelfServiceSaysEnabledAutomatically(t *testing.T) {
	f := newEnablementFixture("", newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)

	ready := f.entitlementReady(t)
	if ready.Status != metav1.ConditionTrue || ready.Reason != servicesv1alpha1.ReasonEntitlementActive {
		t.Errorf("entitlement Ready = %s/%s, want True/%s", ready.Status, ready.Reason, servicesv1alpha1.ReasonEntitlementActive)
	}
	if !strings.Contains(ready.Message, "enabled automatically") {
		t.Errorf("entitlement message %q doesn't say it was enabled automatically", ready.Message)
	}

	cr := consumerReady(t, f.consumer(t))
	if cr.Status != metav1.ConditionTrue || cr.Reason != reasonConsumerEnabled {
		t.Errorf("consumer Ready = %s/%s, want True/%s", cr.Status, cr.Reason, reasonConsumerEnabled)
	}
}

func TestEnablement_PendingConsumerHasReadyCondition(t *testing.T) {
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)

	cr := consumerReady(t, f.consumer(t))
	if cr.Status != metav1.ConditionFalse || cr.Reason != reasonConsumerPendingApproval {
		t.Errorf("consumer Ready = %s/%s, want False/%s", cr.Status, cr.Reason, reasonConsumerPendingApproval)
	}
}

func TestEnablement_RequestMessageReachesProvider(t *testing.T) {
	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Spec.RequestMessage = "Evaluating for our EU rollout."
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, ent)
	f.reconcileEntitlement(t)

	if got := f.consumer(t).Spec.RequestMessage; got != ent.Spec.RequestMessage {
		t.Errorf("consumer.spec.requestMessage = %q, want %q", got, ent.Spec.RequestMessage)
	}
}

// The approval must still read as an approval after the entitlement
// reconciler's next pass; it used to overwrite it with the self-service wording.
func TestEnablement_ApprovalSurvivesEntitlementReconcile(t *testing.T) {
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)
	f.decide(t, servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
		Message:  "Welcome to the early access program.",
	})

	afterDecision := f.entitlementReady(t)
	f.reconcileEntitlement(t)
	ready := f.entitlementReady(t)

	if ready.Status != metav1.ConditionTrue || ready.Reason != servicesv1alpha1.ReasonEntitlementApproved {
		t.Errorf("entitlement Ready = %s/%s, want True/%s", ready.Status, ready.Reason, servicesv1alpha1.ReasonEntitlementApproved)
	}
	if !strings.Contains(ready.Message, "Welcome to the early access program.") {
		t.Errorf("entitlement message %q doesn't carry the provider's note", ready.Message)
	}
	if ready.Message != afterDecision.Message || ready.Reason != afterDecision.Reason {
		t.Errorf("Ready changed across reconcilers: %s %q -> %s %q", afterDecision.Reason, afterDecision.Message, ready.Reason, ready.Message)
	}
	if cr := consumerReady(t, f.consumer(t)); cr.Reason != reasonConsumerApproved {
		t.Errorf("consumer Ready reason = %s, want %s", cr.Reason, reasonConsumerApproved)
	}
}

func TestEnablement_DenialCarriesProviderReason(t *testing.T) {
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)
	f.decide(t, servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionDenied,
		Message:  "Not available in your region yet.",
	})
	f.reconcileEntitlement(t)

	ready := f.entitlementReady(t)
	if ready.Reason != servicesv1alpha1.ReasonEntitlementRejected {
		t.Errorf("entitlement Ready reason = %s, want %s", ready.Reason, servicesv1alpha1.ReasonEntitlementRejected)
	}
	if !strings.Contains(ready.Message, "Not available in your region yet.") {
		t.Errorf("entitlement message %q doesn't carry the provider's reason", ready.Message)
	}
}

func (f *enablementFixture) switchToSelfService(t *testing.T) {
	t.Helper()
	var svc servicesv1alpha1.Service
	if err := f.entitlementR.rootClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	svc.Spec.EnablementPolicy = &servicesv1alpha1.EnablementPolicy{Mode: servicesv1alpha1.EnablementModeSelfService}
	if err := f.entitlementR.rootClient.Update(context.Background(), &svc); err != nil {
		t.Fatalf("switch service to self-service: %v", err)
	}
}

// settle runs the entitlement and consumer reconcilers in turn, as their
// watches on each other's status writes do, and fails if the entitlement's
// Ready condition is still changing once both have run.
func (f *enablementFixture) settle(t *testing.T) *metav1.Condition {
	t.Helper()
	f.reconcileEntitlement(t)
	before := f.entitlementReady(t)
	sc := f.consumer(t)
	if _, err := f.consumerR.Reconcile(context.Background(), entitlementRequest(testProviderProject, sc.Name)); err != nil {
		t.Fatalf("reconcile consumer: %v", err)
	}
	f.reconcileEntitlement(t)
	after := f.entitlementReady(t)
	if before.Reason != after.Reason || before.Message != after.Message {
		t.Fatalf("reconcilers disagree: Ready went %s %q -> %s %q", before.Reason, before.Message, after.Reason, after.Message)
	}
	return after
}

func TestEnablement_SwitchToSelfServiceEnablesPendingRequests(t *testing.T) {
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)
	f.switchToSelfService(t)

	ready := f.settle(t)
	if ready.Status != metav1.ConditionTrue || ready.Reason != servicesv1alpha1.ReasonEntitlementActive {
		t.Errorf("entitlement Ready = %s/%s, want True/%s", ready.Status, ready.Reason, servicesv1alpha1.ReasonEntitlementActive)
	}
}

func TestEnablement_SwitchToSelfServiceKeepsApproval(t *testing.T) {
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)
	f.decide(t, servicesv1alpha1.ProviderApproval{Decision: servicesv1alpha1.ApprovalDecisionApproved})
	f.switchToSelfService(t)

	ready := f.settle(t)
	if ready.Status != metav1.ConditionTrue || ready.Reason != servicesv1alpha1.ReasonEntitlementApproved {
		t.Errorf("entitlement Ready = %s/%s, want True/%s", ready.Status, ready.Reason, servicesv1alpha1.ReasonEntitlementApproved)
	}
}

func TestEnablement_SwitchToSelfServiceKeepsDenial(t *testing.T) {
	f := newEnablementFixture(servicesv1alpha1.EnablementModeGatedByProvider, newEntitlement(testServiceSlug, testServiceSlug))
	f.reconcileEntitlement(t)
	f.decide(t, servicesv1alpha1.ProviderApproval{Decision: servicesv1alpha1.ApprovalDecisionDenied})
	f.switchToSelfService(t)

	ready := f.settle(t)
	if ready.Status != metav1.ConditionFalse || ready.Reason != servicesv1alpha1.ReasonEntitlementRejected {
		t.Errorf("entitlement Ready = %s/%s, want False/%s", ready.Status, ready.Reason, servicesv1alpha1.ReasonEntitlementRejected)
	}
	if got := f.consumer(t).Status.Phase; got != servicesv1alpha1.ConsumerPhaseDenied {
		t.Errorf("consumer phase = %q, want Denied", got)
	}
}
