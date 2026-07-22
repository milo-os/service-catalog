// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	billingapply "go.miloapis.com/billing/applyconfiguration/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// meterCapturingClient records every MeterDefinition Apply so tests can
// assert on the materialized billing object without a live API server.
type meterCapturingClient struct {
	client.Client
	meters []*billingapply.MeterDefinitionApplyConfiguration
}

func (c *meterCapturingClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	if md, ok := obj.(*billingapply.MeterDefinitionApplyConfiguration); ok {
		c.meters = append(c.meters, md)
	}
	return nil
}

func newBillingFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = billingv1alpha1.AddToScheme(s)
	return s
}

// TestApplyMeterDefinitions_DimensionsPassThrough verifies that a metric's
// declared dimensions fan out to MeterDefinition.spec.measurement.dimensions.
// This is the catalog half of per-model assistant pricing: the billing
// validator quarantines events carrying dimension keys the meter does not
// declare, so `model` must be declared here for the producer to emit it.
func TestApplyMeterDefinitions_DimensionsPassThrough(t *testing.T) {
	const (
		meterName = "assistant.miloapis.com/conversation/input-tokens"
		mrtType   = "assistant.miloapis.com/Conversation"
	)

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant-sc", UID: "sc-uid-1"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{
				{
					Name:       meterName,
					Kind:       servicesv1alpha1.MetricKindDelta,
					Unit:       "{token}",
					Dimensions: []string{"model"},
				},
			},
			Billing: &servicesv1alpha1.ServiceBillingConfig{
				ConsumerDestinations: []servicesv1alpha1.BillingConsumerDestination{
					{
						MonitoredResourceType: mrtType,
						Metrics:               []string{meterName},
					},
				},
			},
		},
	}

	scheme := newBillingFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &meterCapturingClient{Client: base}

	fanOut := &BillingFanOut{Client: capturing}

	if _, err := fanOut.applyMeterDefinitions(context.Background(), sc, "assistant.miloapis.com"); err != nil {
		t.Fatalf("applyMeterDefinitions: %v", err)
	}

	if len(capturing.meters) != 1 {
		t.Fatalf("expected exactly one MeterDefinition patch, got %d", len(capturing.meters))
	}

	md := capturing.meters[0]
	dims := md.Spec.Measurement.Dimensions
	if len(dims) != 1 || dims[0] != "model" {
		t.Errorf("MeterDefinition measurement.dimensions = %v, want [model]", dims)
	}
}

// TestApplyMeterDefinitions_NoDimensions verifies the field stays nil/empty
// when a metric declares no dimensions — the common case for meters that
// are not grouped or priced by an attribute.
func TestApplyMeterDefinitions_NoDimensions(t *testing.T) {
	const (
		meterName = "assistant.miloapis.com/conversation/messages"
		mrtType   = "assistant.miloapis.com/Conversation"
	)

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant-sc", UID: "sc-uid-2"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{
				{
					Name: meterName,
					Kind: servicesv1alpha1.MetricKindDelta,
					Unit: "{message}",
				},
			},
			Billing: &servicesv1alpha1.ServiceBillingConfig{
				ConsumerDestinations: []servicesv1alpha1.BillingConsumerDestination{
					{
						MonitoredResourceType: mrtType,
						Metrics:               []string{meterName},
					},
				},
			},
		},
	}

	scheme := newBillingFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &meterCapturingClient{Client: base}

	fanOut := &BillingFanOut{Client: capturing}

	if _, err := fanOut.applyMeterDefinitions(context.Background(), sc, "assistant.miloapis.com"); err != nil {
		t.Fatalf("applyMeterDefinitions: %v", err)
	}

	if len(capturing.meters) != 1 {
		t.Fatalf("expected exactly one MeterDefinition patch, got %d", len(capturing.meters))
	}

	if dims := capturing.meters[0].Spec.Measurement.Dimensions; len(dims) != 0 {
		t.Errorf("MeterDefinition measurement.dimensions = %v, want empty", dims)
	}
}
