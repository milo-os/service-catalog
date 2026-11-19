// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func predicateEntitlement() *servicesv1alpha1.ServiceEntitlement {
	e := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Generation: 1, ResourceVersion: "1"},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}},
	}
	e.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
	apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "Active",
	})
	return e
}

func TestEntitlementWatchPredicates(t *testing.T) {
	cases := []struct {
		name                     string
		mutate                   func(*servicesv1alpha1.ServiceEntitlement)
		entitlement, provisioner bool
		capability               bool
	}{
		{
			name:   "resourceVersion only",
			mutate: func(e *servicesv1alpha1.ServiceEntitlement) {},
		},
		{
			name: "provisioning ledger and heartbeat",
			mutate: func(e *servicesv1alpha1.ServiceEntitlement) {
				now := metav1.Now()
				e.Status.LastProvisioningEvaluation = &now
				e.Status.ProvisionedResources = []servicesv1alpha1.ProvisionedResourceStatus{{Name: "classes"}}
				apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
					Type: servicesv1alpha1.ConditionTypeProvisioned, Status: metav1.ConditionTrue, Reason: "Provisioned",
				})
			},
		},
		{
			name: "setup progress",
			mutate: func(e *servicesv1alpha1.ServiceEntitlement) {
				e.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{}
				apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
					Type: servicesv1alpha1.ConditionTypeSetupComplete, Status: metav1.ConditionTrue, Reason: "SetupComplete",
				})
			},
		},
		{
			name: "provider decision on Ready",
			mutate: func(e *servicesv1alpha1.ServiceEntitlement) {
				apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
					Type: ConditionTypeReady, Status: metav1.ConditionFalse, Reason: "Denied",
				})
			},
			entitlement: true,
		},
		{
			name: "phase",
			mutate: func(e *servicesv1alpha1.ServiceEntitlement) {
				e.Status.Phase = servicesv1alpha1.EntitlementPhasePendingApproval
			},
			entitlement: true, provisioner: true, capability: true,
		},
		{
			name: "deletion",
			mutate: func(e *servicesv1alpha1.ServiceEntitlement) {
				now := metav1.Now()
				e.DeletionTimestamp = &now
			},
			entitlement: true, provisioner: true, capability: true,
		},
		{
			name:        "spec",
			mutate:      func(e *servicesv1alpha1.ServiceEntitlement) { e.Generation++; e.Spec.RequestMessage = "please" },
			entitlement: true, provisioner: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := predicateEntitlement()
			after := before.DeepCopy()
			after.ResourceVersion = "2"
			tc.mutate(after)
			ev := event.UpdateEvent{ObjectOld: before, ObjectNew: after}
			if got := serviceEntitlementPredicate().Update(ev); got != tc.entitlement {
				t.Errorf("service-entitlement passes = %v, want %v", got, tc.entitlement)
			}
			if got := provisioningEntitlementPredicate().Update(ev); got != tc.provisioner {
				t.Errorf("service-provisioning passes = %v, want %v", got, tc.provisioner)
			}
			if got := capabilityEntitlementPredicate().Update(ev); got != tc.capability {
				t.Errorf("capability-binding passes = %v, want %v", got, tc.capability)
			}
		})
	}
}

func TestServiceConfigurationPredicate(t *testing.T) {
	before := &servicesv1alpha1.ServiceConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "compute-v1", Generation: 1}}
	before.Status.ServiceName = "compute.miloapis.com"

	progress := before.DeepCopy()
	apimeta.SetStatusCondition(&progress.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "Reconciled",
	})
	renamed := before.DeepCopy()
	renamed.Status.ServiceName = "compute.example.com"
	edited := before.DeepCopy()
	edited.Generation++

	p := serviceConfigurationPredicate()
	for name, tc := range map[string]struct {
		after *servicesv1alpha1.ServiceConfiguration
		want  bool
	}{
		"status progress":        {progress, false},
		"canonical service name": {renamed, true},
		"spec edit":              {edited, true},
	} {
		got := p.Update(event.TypedUpdateEvent[*servicesv1alpha1.ServiceConfiguration]{ObjectOld: before, ObjectNew: tc.after})
		if got != tc.want {
			t.Errorf("%s: passes = %v, want %v", name, got, tc.want)
		}
	}
}

func TestBillingEntitlementPredicate(t *testing.T) {
	before := &billingv1alpha1.BillingEntitlement{ObjectMeta: metav1.ObjectMeta{Name: "be", Generation: 1}}

	other := before.DeepCopy()
	apimeta.SetStatusCondition(&other.Status.Conditions, metav1.Condition{
		Type: "Synced", Status: metav1.ConditionTrue, Reason: "Synced",
	})
	ready := before.DeepCopy()
	apimeta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "Ready",
	})

	p := billingEntitlementPredicate()
	if p.Update(event.TypedUpdateEvent[*billingv1alpha1.BillingEntitlement]{ObjectOld: before, ObjectNew: other}) {
		t.Error("a condition quota grants do not read passed")
	}
	if !p.Update(event.TypedUpdateEvent[*billingv1alpha1.BillingEntitlement]{ObjectOld: before, ObjectNew: ready}) {
		t.Error("becoming Ready was dropped")
	}
}
