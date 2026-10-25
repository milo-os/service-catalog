// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Setup has several controllers writing one entitlement's status within
// seconds. Each works from its own cached copy, and none may fail or erase
// another's condition because the copy is stale.
func TestEntitlementStatusWritersDoNotConflict(t *testing.T) {
	ctx := context.Background()
	consumer := newFakeClient(&servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "compute"},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}},
	})
	var stale servicesv1alpha1.ServiceEntitlement
	if err := consumer.Get(ctx, client.ObjectKey{Name: "compute"}, &stale); err != nil {
		t.Fatal(err)
	}

	writers := []func(e *servicesv1alpha1.ServiceEntitlement) error{
		func(e *servicesv1alpha1.ServiceEntitlement) error {
			return applyEntitlementReadyStatus(ctx, consumer, e, servicesv1alpha1.EntitlementPhaseActive, "Enabled", "Enabled.", "compute.miloapis.com")
		},
		func(e *servicesv1alpha1.ServiceEntitlement) error {
			return patchEntitlementSuspendedCondition(ctx, consumer, e, projectSuspension{Suspended: true})
		},
		func(e *servicesv1alpha1.ServiceEntitlement) error {
			before := e.DeepCopy()
			apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
				Type: servicesv1alpha1.ConditionTypeProvisioned, Status: metav1.ConditionTrue, Reason: servicesv1alpha1.ReasonProvisioned,
			})
			return applyEntitlementStatus(ctx, consumer, provisioningStatusOwner, before, e)
		},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(writers))
	for i, write := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = write(stale.DeepCopy())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumer.Get(ctx, client.ObjectKey{Name: "compute"}, &got); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{ConditionTypeReady, ConditionTypeSuspended, servicesv1alpha1.ConditionTypeProvisioned} {
		if !apimeta.IsStatusConditionTrue(got.Status.Conditions, typ) {
			t.Errorf("%s = %#v, want True", typ, apimeta.FindStatusCondition(got.Status.Conditions, typ))
		}
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhaseActive || got.Status.ServiceName != "compute.miloapis.com" {
		t.Errorf("status = %+v, want the Ready writer's fields", got.Status)
	}
}

// Retiring a condition only works once its new owner holds it outright, so
// fields an Update manager wrote are split between the apply managers that own
// them now, and anything unmapped stays put.
func TestMigratedStatusManagedFieldsSplitsByOwner(t *testing.T) {
	legacy := metav1.ManagedFieldsEntry{
		Manager:     "services",
		Operation:   metav1.ManagedFieldsOperationUpdate,
		Subresource: "status",
		APIVersion:  "services.miloapis.com/v1alpha1",
		FieldsType:  "FieldsV1",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{".":{},"f:phase":{},"f:setup":{".":{},"f:requestedAt":{}},` +
			`"f:conditions":{".":{},` +
			`"k:{\"type\":\"Ready\"}":{".":{},"f:status":{},"f:type":{}},` +
			`"k:{\"type\":\"Suspended\"}":{".":{},"f:status":{},"f:type":{}},` +
			`"k:{\"type\":\"Custom\"}":{".":{},"f:status":{},"f:type":{}}}}}`)},
	}
	spec := metav1.ManagedFieldsEntry{Manager: "services", Operation: metav1.ManagedFieldsOperationUpdate, FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:serviceRef":{"f:name":{}}}}`)}}

	out, changed, err := migratedStatusManagedFields([]metav1.ManagedFieldsEntry{spec, legacy})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	byManager := map[string]string{}
	for _, e := range out {
		byManager[e.Manager+"/"+string(e.Operation)+"/"+e.Subresource] = e.FieldsV1.GetRawString()
	}
	if got := byManager["services/Update/"]; got != spec.FieldsV1.GetRawString() {
		t.Errorf("spec ownership changed: %s", got)
	}
	left := byManager["services/Update/status"]
	if !strings.Contains(left, `Custom`) || strings.Contains(left, `Ready`) || strings.Contains(left, `f:phase`) || strings.Contains(left, `f:setup`) {
		t.Errorf("legacy manager kept %s, want only the unmapped condition", left)
	}
	for manager, want := range map[string][]string{
		entitlementReadyOwner.manager:   {`f:phase`, `Ready`},
		suspendedConditionOwner.manager: {`Suspended`},
		setupStatusOwner.manager:        {`f:setup`, `f:requestedAt`},
	} {
		got, ok := byManager[manager+"/Apply/status"]
		if !ok {
			t.Errorf("%s has no apply entry", manager)
			continue
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s owns %s, want it to include %s", manager, got, w)
			}
		}
	}

	if _, changed, _ := migratedStatusManagedFields(out); changed {
		t.Error("migration is not idempotent")
	}
}
