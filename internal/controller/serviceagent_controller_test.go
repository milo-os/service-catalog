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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// newAgentFakeClient builds a fake client with status-subresource
// support for the agent catalog kinds.
func newAgentFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(
			&servicesv1alpha1.ServiceAgent{},
			&servicesv1alpha1.ServiceAgentConfiguration{},
		).
		Build()
}

func newTestServiceAgent(name, serviceRef string, phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgent {
	return &servicesv1alpha1.ServiceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceAgentSpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: serviceRef},
			Phase:       phase,
			DisplayName: "Test Agent",
		},
	}
}

// reconcileAgentTwice drives the add-finalizer pass and the status pass.
func reconcileAgentTwice(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &ServiceAgentReconciler{client: c}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile iter %d: %v", i, err)
		}
	}
}

func TestServiceAgentReconciler_DraftConditions(t *testing.T) {
	c := newAgentFakeClient(newTestServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhaseDraft))
	reconcileAgentTwice(t, c, "streaming-agent")

	var got servicesv1alpha1.ServiceAgent
	if err := c.Get(context.Background(), types.NamespacedName{Name: "streaming-agent"}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if !hasFinalizer(&got, serviceAgentFinalizer) {
		t.Errorf("expected finalizer %q on agent", serviceAgentFinalizer)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeReady) {
		t.Errorf("expected Ready=True on Draft agent")
	}
	if apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypePublished) {
		t.Errorf("expected Published=False on Draft agent")
	}
	if got.Status.PublishedAt != nil {
		t.Errorf("expected publishedAt to be unset on Draft agent")
	}
}

func TestServiceAgentReconciler_PublishedStampsPublishedAt(t *testing.T) {
	c := newAgentFakeClient(newTestServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhasePublished))
	reconcileAgentTwice(t, c, "streaming-agent")

	var got servicesv1alpha1.ServiceAgent
	if err := c.Get(context.Background(), types.NamespacedName{Name: "streaming-agent"}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypePublished) {
		t.Errorf("expected Published=True on Published agent")
	}
	if got.Status.PublishedAt == nil {
		t.Errorf("expected publishedAt to be stamped on Published agent")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}
