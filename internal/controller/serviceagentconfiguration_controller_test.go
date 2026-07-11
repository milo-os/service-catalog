// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newTestAgentConfiguration(name, agentRef, version string, phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgentConfiguration {
	return &servicesv1alpha1.ServiceAgentConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
			ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: agentRef},
			Version:         version,
			Phase:           phase,
			Metrics: []servicesv1alpha1.MetricSpec{{
				Name: "streaming.streamco.example/agent/tool-invocations",
				Kind: servicesv1alpha1.MetricKindDelta,
				Unit: "{invocation}",
			}},
		},
	}
}

func reconcileAgentConfiguration(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &ServiceAgentConfigurationReconciler{Client: c, Scheme: testScheme()}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestServiceAgentConfigurationReconciler_PublishedResolvesChain(t *testing.T) {
	svc := newPublishedService("streaming", "streaming.streamco.example", testProviderProject, "")
	agent := newTestServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhasePublished)
	sac := newTestAgentConfiguration("streaming-agent-v1", "streaming-agent", "v1", servicesv1alpha1.PhasePublished)
	c := newAgentFakeClient(svc, agent, sac)

	reconcileAgentConfiguration(t, c, "streaming-agent-v1")

	var got servicesv1alpha1.ServiceAgentConfiguration
	if err := c.Get(context.Background(), types.NamespacedName{Name: "streaming-agent-v1"}, &got); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got.Status.ServiceName != "streaming.streamco.example" {
		t.Errorf("status.serviceName = %q, want streaming.streamco.example", got.Status.ServiceName)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeReady) {
		t.Errorf("expected Ready=True")
	}
	fanOut := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeMetricsFanOutHealthy)
	if fanOut == nil {
		t.Fatalf("expected MetricsFanOutHealthy condition")
	}
	if fanOut.Status != metav1.ConditionTrue || fanOut.Reason != reasonMetricsFanOutStubbed {
		t.Errorf("MetricsFanOutHealthy = %s/%s, want True/%s", fanOut.Status, fanOut.Reason, reasonMetricsFanOutStubbed)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypePublished) {
		t.Errorf("expected Published=True")
	}
	if got.Status.PublishedAt == nil {
		t.Errorf("expected publishedAt to be stamped")
	}
}

func TestServiceAgentConfigurationReconciler_DraftSkipsFanOut(t *testing.T) {
	svc := newPublishedService("streaming", "streaming.streamco.example", testProviderProject, "")
	agent := newTestServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhasePublished)
	sac := newTestAgentConfiguration("streaming-agent-v1", "streaming-agent", "v1", servicesv1alpha1.PhaseDraft)
	c := newAgentFakeClient(svc, agent, sac)

	reconcileAgentConfiguration(t, c, "streaming-agent-v1")

	var got servicesv1alpha1.ServiceAgentConfiguration
	if err := c.Get(context.Background(), types.NamespacedName{Name: "streaming-agent-v1"}, &got); err != nil {
		t.Fatalf("get config: %v", err)
	}
	fanOut := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeMetricsFanOutHealthy)
	if fanOut == nil {
		t.Fatalf("expected MetricsFanOutHealthy condition")
	}
	if fanOut.Status != metav1.ConditionTrue || fanOut.Reason != reasonMetricsFanOutSkipped {
		t.Errorf("MetricsFanOutHealthy = %s/%s, want True/%s", fanOut.Status, fanOut.Reason, reasonMetricsFanOutSkipped)
	}
	if got.Status.PublishedAt != nil {
		t.Errorf("expected publishedAt unset for Draft")
	}
}

func TestServiceAgentConfigurationReconciler_MissingAgentNotReady(t *testing.T) {
	sac := newTestAgentConfiguration("streaming-agent-v1", "missing-agent", "v1", servicesv1alpha1.PhasePublished)
	c := newAgentFakeClient(sac)

	reconcileAgentConfiguration(t, c, "streaming-agent-v1")

	var got servicesv1alpha1.ServiceAgentConfiguration
	if err := c.Get(context.Background(), types.NamespacedName{Name: "streaming-agent-v1"}, &got); err != nil {
		t.Fatalf("get config: %v", err)
	}
	ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonServiceAgentRefNotFound {
		t.Errorf("Ready = %+v, want False/%s", ready, reasonServiceAgentRefNotFound)
	}
	fanOut := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeMetricsFanOutHealthy)
	if fanOut == nil || fanOut.Status != metav1.ConditionFalse {
		t.Errorf("MetricsFanOutHealthy = %+v, want False when agent missing", fanOut)
	}
}
