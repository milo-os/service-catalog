// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
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
	// Entitlement lives in the consumer cluster, keyed by service slug.
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
	ent := newEntitlement(testServiceSlug, testServiceSlug)

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
