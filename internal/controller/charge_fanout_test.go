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

type servicePricingCapturingClient struct {
	client.Client
	pricings []*billingapply.ServicePricingApplyConfiguration
}

func (c *servicePricingCapturingClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	if sp, ok := obj.(*billingapply.ServicePricingApplyConfiguration); ok {
		c.pricings = append(c.pricings, sp)
	}
	return nil
}

func newChargeFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = billingv1alpha1.AddToScheme(s)
	return s
}

func TestChargeFanOut_ApplyUsageAndFixed(t *testing.T) {
	const (
		metricName  = "compute.datumapis.com/instance/cpu-allocated"
		serviceName = "compute.datumapis.com"
	)

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-sc", UID: "sc-uid-charges"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{
				{
					Name:        metricName,
					DisplayName: "CPU Allocated",
					Kind:        servicesv1alpha1.MetricKindGauge,
					Unit:        "1",
				},
				{
					Name: "compute.datumapis.com/instance/unpriced",
					Kind: servicesv1alpha1.MetricKindDelta,
					Unit: "s",
				},
			},
			Charges: []servicesv1alpha1.ServiceChargeSpec{
				{
					Name:        metricName,
					ChargeType:  servicesv1alpha1.ServiceChargeTypeUsage,
					DisplayName: "CPU Allocated",
					Currency:    "USD",
					MetricRef:   metricName,
					PricingUnit: "vcpu",
					Rates: []servicesv1alpha1.PricingRateEntry{
						{
							Match: &servicesv1alpha1.DimensionMatch{Dimension: "tier", Value: "standard"},
							Flat:  "0.025",
						},
						{Flat: "0.030"},
					},
				},
				{
					Name:        "compute.datumapis.com/platform-fee",
					ChargeType:  servicesv1alpha1.ServiceChargeTypeRecurring,
					DisplayName: "Platform Fee",
					Currency:    "USD",
					Amount:      "5.00",
					Interval:    servicesv1alpha1.ChargeIntervalMonthly,
				},
			},
			Billing: &servicesv1alpha1.ServiceBillingConfig{
				QuotaGating: servicesv1alpha1.QuotaGatingBillingEntitlement,
			},
		},
	}

	scheme := newChargeFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &servicePricingCapturingClient{Client: base}
	fanOut := &ChargeFanOut{Client: capturing}

	desired, err := fanOut.applyServicePricings(context.Background(), sc, serviceName)
	if err != nil {
		t.Fatalf("applyServicePricings: %v", err)
	}

	wantUsage := "compute-datumapis-com--instance-cpu-allocated"
	wantFee := "compute-datumapis-com--platform-fee"
	if len(desired) != 2 {
		t.Fatalf("desired count = %d, want 2", len(desired))
	}
	if _, ok := desired[wantUsage]; !ok {
		t.Fatalf("desired missing %q: %v", wantUsage, desired)
	}
	if _, ok := desired[wantFee]; !ok {
		t.Fatalf("desired missing %q: %v", wantFee, desired)
	}
	if len(capturing.pricings) != 2 {
		t.Fatalf("expected 2 ServicePricing applies, got %d", len(capturing.pricings))
	}

	var usage *billingapply.ServicePricingApplyConfiguration
	for _, sp := range capturing.pricings {
		if sp.Name != nil && *sp.Name == wantUsage {
			usage = sp
			break
		}
	}
	if usage == nil || usage.Spec == nil {
		t.Fatal("missing Usage ServicePricing apply")
	}
	if usage.Spec.ChargeType == nil || *usage.Spec.ChargeType != billingv1alpha1.ChargeTypeUsage {
		t.Errorf("chargeType = %v, want Usage", usage.Spec.ChargeType)
	}
	if usage.Spec.Metric == nil || *usage.Spec.Metric != metricName {
		t.Errorf("metric = %v, want %q", usage.Spec.Metric, metricName)
	}
	if usage.Spec.PricingUnit == nil || *usage.Spec.PricingUnit != "vcpu" {
		t.Errorf("pricingUnit = %v, want vcpu", usage.Spec.PricingUnit)
	}
}
