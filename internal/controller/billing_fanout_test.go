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
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// meterCapturingClient records every MeterDefinition Patch so tests can
// assert on the materialized billing object without a live API server.
type meterCapturingClient struct {
	client.Client
	meters []*billingv1alpha1.MeterDefinition
}

func (c *meterCapturingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if md, ok := obj.(*billingv1alpha1.MeterDefinition); ok {
		c.meters = append(c.meters, md.DeepCopy())
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

	fanOut := &BillingFanOut{Client: capturing, Scheme: scheme}

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

	fanOut := &BillingFanOut{Client: capturing, Scheme: scheme}

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

func TestApplyMeterDefinitions_RecreatesOnDimensionShrink(t *testing.T) {
	const (
		meterName = "networking.datumapis.com/gateway/requests"
		mrtType   = "networking.datumapis.com/HTTPRoute"
		mdName    = "networking-datumapis-com-gateway-requests"
	)

	existing := &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: mdName},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName: meterName,
			Measurement: billingv1alpha1.MeterMeasurement{
				Dimensions: []string{"gateway", "gateway_namespace", "region"},
			},
		},
	}

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "networking-sc", UID: "sc-uid-3"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{
				{
					Name:       meterName,
					Kind:       servicesv1alpha1.MetricKindDelta,
					Unit:       "{request}",
					Dimensions: []string{"region", "gateway_class"},
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	fanOut := &BillingFanOut{Client: cl, Scheme: scheme}

	if _, err := fanOut.applyMeterDefinitions(context.Background(), sc, "networking.datumapis.com"); err != nil {
		t.Fatalf("applyMeterDefinitions: %v", err)
	}

	got := &billingv1alpha1.MeterDefinition{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: mdName}, got); err != nil {
		t.Fatalf("get MeterDefinition after apply: %v", err)
	}

	wantDims := []string{"region", "gateway_class"}
	if len(got.Spec.Measurement.Dimensions) != len(wantDims) {
		t.Fatalf("dimensions = %v, want %v", got.Spec.Measurement.Dimensions, wantDims)
	}
	for i, dim := range wantDims {
		if got.Spec.Measurement.Dimensions[i] != dim {
			t.Fatalf("dimensions = %v, want %v", got.Spec.Measurement.Dimensions, wantDims)
		}
	}
}

func TestApplyMonitoredResourceTypes_RecreatesOnLabelShrink(t *testing.T) {
	const (
		mrtTypeName = "networking.datumapis.com/HTTPRoute"
		mrtName     = "networking-datumapis-com-httproute"
	)

	existing := &billingv1alpha1.MonitoredResourceType{
		ObjectMeta: metav1.ObjectMeta{Name: mrtName},
		Spec: billingv1alpha1.MonitoredResourceTypeSpec{
			ResourceTypeName: mrtTypeName,
			Labels: []billingv1alpha1.MonitoredResourceLabel{
				{Name: "gateway"},
				{Name: "gateway_namespace"},
				{Name: "region"},
			},
		},
	}

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "networking-sc", UID: "sc-uid-4"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			MonitoredResourceTypes: []servicesv1alpha1.MonitoredResourceTypeSpec{
				{
					Type: mrtTypeName,
					GVK: servicesv1alpha1.GVKRef{
						Group: "gateway.networking.k8s.io",
						Kind:  "HTTPRoute",
					},
					Labels: []servicesv1alpha1.MonitoredResourceLabel{
						{Name: "region"},
						{Name: "gateway_class"},
					},
				},
			},
		},
	}

	scheme := newBillingFanOutScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	fanOut := &BillingFanOut{Client: cl, Scheme: scheme}

	if _, err := fanOut.applyMonitoredResourceTypes(context.Background(), sc, "networking.datumapis.com"); err != nil {
		t.Fatalf("applyMonitoredResourceTypes: %v", err)
	}

	got := &billingv1alpha1.MonitoredResourceType{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: mrtName}, got); err != nil {
		t.Fatalf("get MonitoredResourceType after apply: %v", err)
	}

	wantLabels := []string{"region", "gateway_class"}
	if len(got.Spec.Labels) != len(wantLabels) {
		t.Fatalf("labels = %v, want names %v", got.Spec.Labels, wantLabels)
	}
	for i, name := range wantLabels {
		if got.Spec.Labels[i].Name != name {
			t.Fatalf("labels = %v, want names %v", got.Spec.Labels, wantLabels)
		}
	}
}
