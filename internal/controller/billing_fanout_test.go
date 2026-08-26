// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestDimensionsShrink(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		desired  []string
		want     bool
	}{
		{name: "no existing", existing: nil, desired: []string{"region"}, want: false},
		{name: "additive", existing: []string{"region"}, desired: []string{"region", "project"}, want: false},
		{name: "unchanged", existing: []string{"region", "gateway"}, desired: []string{"region", "gateway"}, want: false},
		{name: "reordered", existing: []string{"gateway", "region"}, desired: []string{"region", "gateway"}, want: false},
		{name: "subtractive", existing: []string{"gateway", "region"}, desired: []string{"region"}, want: true},
		{name: "cleared", existing: []string{"gateway"}, desired: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dimensionsShrink(tt.existing, tt.desired); got != tt.want {
				t.Errorf("dimensionsShrink(%v, %v) = %v, want %v", tt.existing, tt.desired, got, tt.want)
			}
		})
	}
}

func TestDeleteMeterDefinitionIfDimensionsShrink(t *testing.T) {
	const mdName = "networking-datumapis-com-gateway-requests"

	existing := &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: mdName},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			Measurement: billingv1alpha1.MeterMeasurement{
				Dimensions: []string{"gateway", "region"},
			},
		},
	}

	scheme := newBillingFanOutScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	fanOut := &BillingFanOut{Client: cl}

	if err := fanOut.deleteMeterDefinitionIfDimensionsShrink(
		context.Background(),
		mdName,
		[]string{"region"},
	); err != nil {
		t.Fatalf("deleteMeterDefinitionIfDimensionsShrink: %v", err)
	}

	err := cl.Get(context.Background(), client.ObjectKey{Name: mdName}, &billingv1alpha1.MeterDefinition{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected MeterDefinition to be deleted, get err = %v", err)
	}
}

func TestDeleteMonitoredResourceTypeIfLabelsShrink(t *testing.T) {
	const mrtName = "networking-datumapis-com-httproute"

	existing := &billingv1alpha1.MonitoredResourceType{
		ObjectMeta: metav1.ObjectMeta{Name: mrtName},
		Spec: billingv1alpha1.MonitoredResourceTypeSpec{
			Labels: []billingv1alpha1.MonitoredResourceLabel{
				{Name: "gateway"},
				{Name: "region"},
			},
		},
	}

	scheme := newBillingFanOutScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	fanOut := &BillingFanOut{Client: cl}

	if err := fanOut.deleteMonitoredResourceTypeIfLabelsShrink(
		context.Background(),
		mrtName,
		[]servicesv1alpha1.MonitoredResourceLabel{{Name: "region"}},
	); err != nil {
		t.Fatalf("deleteMonitoredResourceTypeIfLabelsShrink: %v", err)
	}

	err := cl.Get(context.Background(), client.ObjectKey{Name: mrtName}, &billingv1alpha1.MonitoredResourceType{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected MonitoredResourceType to be deleted, get err = %v", err)
	}
}
