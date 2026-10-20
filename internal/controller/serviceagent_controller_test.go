// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// newServiceAgentReconciler returns the reconciler, its client, and a counter
// of status writes.
func newServiceAgentReconciler(t *testing.T, allowed map[string]struct{}, objs ...client.Object) (*ServiceAgentReconciler, client.Client, *int) {
	t.Helper()
	writes := new(int)
	c := fake.NewClientBuilder().
		WithScheme(capabilityScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&servicesv1alpha1.ServiceAgent{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				*writes++
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				*writes++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	return &ServiceAgentReconciler{client: c, EntitlementFreeAgents: allowed}, c, writes
}

func reconcileAgent(t *testing.T, r *ServiceAgentReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: cbAgent}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func entitlementWaived(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var agent servicesv1alpha1.ServiceAgent
	if err := c.Get(context.Background(), types.NamespacedName{Name: cbAgent}, &agent); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	return apimeta.FindStatusCondition(agent.Status.Conditions, servicesv1alpha1.ConditionTypeEntitlementWaived)
}

func TestServiceAgent_AllowlistedNoneIsWaived(t *testing.T) {
	r, c, _ := newServiceAgentReconciler(t, allowlist(cbAgent),
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone))
	reconcileAgent(t, r)

	cond := entitlementWaived(t, c)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != servicesv1alpha1.ReasonAllowlisted {
		t.Errorf("EntitlementWaived = %+v, want True/%s", cond, servicesv1alpha1.ReasonAllowlisted)
	}
}

// A provider that asks for None without being allowlisted is told so on its
// own agent, rather than left to wonder why unentitled projects never get it.
func TestServiceAgent_NoneNotAllowlistedIsReported(t *testing.T) {
	r, c, writes := newServiceAgentReconciler(t, allowlist(),
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone))
	reconcileAgent(t, r)

	cond := entitlementWaived(t, c)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != servicesv1alpha1.ReasonNotAllowlisted {
		t.Fatalf("EntitlementWaived = %+v, want False/%s", cond, servicesv1alpha1.ReasonNotAllowlisted)
	}

	// Written on transition only: the same answer again writes nothing.
	*writes = 0
	reconcileAgent(t, r)
	if *writes != 0 {
		t.Errorf("unchanged answer made %d status writes, want 0", *writes)
	}
}

// An agent that asks for Required has nothing to be answered, and setting an
// agent back to Required clears an earlier answer.
func TestServiceAgent_RequiredCarriesNoCondition(t *testing.T) {
	r, c, writes := newServiceAgentReconciler(t, allowlist(cbAgent),
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementRequired))
	reconcileAgent(t, r)
	if cond := entitlementWaived(t, c); cond != nil {
		t.Errorf("EntitlementWaived = %+v, want absent", cond)
	}
	if *writes != 0 {
		t.Errorf("made %d status writes for an agent with nothing to report, want 0", *writes)
	}

	var agent servicesv1alpha1.ServiceAgent
	if err := c.Get(context.Background(), types.NamespacedName{Name: cbAgent}, &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Visibility.Entitlement = servicesv1alpha1.VisibilityEntitlementNone
	if err := c.Update(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	reconcileAgent(t, r)
	if cond := entitlementWaived(t, c); cond == nil {
		t.Fatal("expected EntitlementWaived once the agent asks for None")
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: cbAgent}, &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Visibility.Entitlement = servicesv1alpha1.VisibilityEntitlementRequired
	if err := c.Update(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	reconcileAgent(t, r)
	if cond := entitlementWaived(t, c); cond != nil {
		t.Errorf("EntitlementWaived = %+v, want cleared after setting Required", cond)
	}
}
