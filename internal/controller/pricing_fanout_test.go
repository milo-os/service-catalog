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

func newPricingFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = billingv1alpha1.AddToScheme(s)
	return s
}

func TestPricingFanOut_ApplyUsageServicePricing(t *testing.T) {
	const (
		metricName  = "compute.datumapis.com/instance/cpu-allocated"
		serviceName = "compute.datumapis.com"
	)

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-sc", UID: "sc-uid-pricing"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{
				{
					Name:        metricName,
					DisplayName: "CPU Allocated",
					Kind:        servicesv1alpha1.MetricKindGauge,
					Unit:        "1",
					Pricing: &servicesv1alpha1.MetricPricing{
						Currency:    "USD",
						PricingUnit: "vcpu",
						Rates: []servicesv1alpha1.PricingRateEntry{
							{
								Match: &servicesv1alpha1.DimensionMatch{Dimension: "tier", Value: "standard"},
								Flat:  "0.025",
							},
							{Flat: "0.030"},
						},
					},
				},
				{
					// Unpriced metric — must not emit a ServicePricing.
					Name: "compute.datumapis.com/instance/unpriced",
					Kind: servicesv1alpha1.MetricKindDelta,
					Unit: "s",
				},
			},
			Billing: &servicesv1alpha1.ServiceBillingConfig{
				QuotaGating: servicesv1alpha1.QuotaGatingBillingEntitlement,
			},
		},
	}

	scheme := newPricingFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &servicePricingCapturingClient{Client: base}
	fanOut := &PricingFanOut{Client: capturing}

	desired, err := fanOut.applyServicePricings(context.Background(), sc, serviceName)
	if err != nil {
		t.Fatalf("applyServicePricings: %v", err)
	}

	wantName := "compute-datumapis-com--instance-cpu-allocated"
	if len(desired) != 1 {
		t.Fatalf("desired count = %d, want 1", len(desired))
	}
	if _, ok := desired[wantName]; !ok {
		t.Fatalf("desired missing %q: %v", wantName, desired)
	}
	if len(capturing.pricings) != 1 {
		t.Fatalf("expected 1 ServicePricing apply, got %d", len(capturing.pricings))
	}

	sp := capturing.pricings[0]
	if sp.Name == nil || *sp.Name != wantName {
		t.Errorf("name = %v, want %q", sp.Name, wantName)
	}
	if sp.Namespace == nil || *sp.Namespace != billingv1alpha1.DefaultServicePricingNamespace {
		t.Errorf("namespace = %v, want %q", sp.Namespace, billingv1alpha1.DefaultServicePricingNamespace)
	}
	if sp.Spec == nil || sp.Spec.ChargeType == nil || *sp.Spec.ChargeType != billingv1alpha1.ChargeTypeUsage {
		t.Errorf("chargeType = %v, want Usage", sp.Spec)
	}
	if sp.Spec.ServiceRef == nil || *sp.Spec.ServiceRef != serviceName {
		t.Errorf("serviceRef = %v, want %q", sp.Spec.ServiceRef, serviceName)
	}
	if sp.Spec.Metric == nil || *sp.Spec.Metric != metricName {
		t.Errorf("metric = %v, want %q", sp.Spec.Metric, metricName)
	}
	if sp.Spec.PricingUnit == nil || *sp.Spec.PricingUnit != "vcpu" {
		t.Errorf("pricingUnit = %v, want vcpu", sp.Spec.PricingUnit)
	}
	if sp.Spec.DisplayName == nil || *sp.Spec.DisplayName != "CPU Allocated" {
		t.Errorf("displayName = %v, want CPU Allocated", sp.Spec.DisplayName)
	}
	if len(sp.Spec.Rates) != 2 {
		t.Fatalf("rates len = %d, want 2", len(sp.Spec.Rates))
	}
	if sp.Spec.Rates[0].Match == nil ||
		sp.Spec.Rates[0].Match.Dimension == nil ||
		*sp.Spec.Rates[0].Match.Dimension != "tier" {
		t.Errorf("rates[0].match = %+v, want tier/standard", sp.Spec.Rates[0].Match)
	}
	if sp.Labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("managed-by label = %q, want %q", sp.Labels[labelManagedBy], labelManagedByValue)
	}
	if sp.Labels[labelOwnerService] != serviceName {
		t.Errorf("owner-service label = %q, want %q", sp.Labels[labelOwnerService], serviceName)
	}
}

func TestChargeFanOut_ApplyOneTimeAndRecurring(t *testing.T) {
	const serviceName = "compute.datumapis.com"

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-sc", UID: "sc-uid-charges"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Charges: []servicesv1alpha1.ServiceChargeSpec{
				{
					Name:        "compute.datumapis.com/instance/setup-fee",
					ChargeType:  servicesv1alpha1.ServiceChargeTypeOneTime,
					DisplayName: "Setup Fee",
					Currency:    "USD",
					Amount:      "10.00",
					Trigger:     servicesv1alpha1.ChargeTriggerBillingAccountActivation,
				},
				{
					Name:       "compute.datumapis.com/platform-fee",
					ChargeType: servicesv1alpha1.ServiceChargeTypeRecurring,
					Amount:     "5.00",
					Interval:   servicesv1alpha1.ChargeIntervalMonthly,
				},
			},
		},
	}

	scheme := newPricingFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &servicePricingCapturingClient{Client: base}
	fanOut := &ChargeFanOut{Client: capturing}

	desired, err := fanOut.applyServicePricings(context.Background(), sc, serviceName)
	if err != nil {
		t.Fatalf("applyServicePricings: %v", err)
	}
	if len(desired) != 2 {
		t.Fatalf("desired count = %d, want 2", len(desired))
	}
	if len(capturing.pricings) != 2 {
		t.Fatalf("expected 2 ServicePricing applies, got %d", len(capturing.pricings))
	}

	byName := map[string]*billingapply.ServicePricingApplyConfiguration{}
	for _, sp := range capturing.pricings {
		if sp.Name != nil {
			byName[*sp.Name] = sp
		}
	}

	setup := byName["compute-datumapis-com--instance-setup-fee"]
	if setup == nil || setup.Spec == nil {
		t.Fatal("missing setup-fee ServicePricing")
	}
	if setup.Spec.ChargeType == nil || *setup.Spec.ChargeType != billingv1alpha1.ChargeTypeOneTime {
		t.Errorf("setup chargeType = %v, want OneTime", setup.Spec.ChargeType)
	}
	if setup.Spec.Amount == nil || *setup.Spec.Amount != "10.00" {
		t.Errorf("setup amount = %v, want 10.00", setup.Spec.Amount)
	}
	if setup.Spec.Trigger == nil || *setup.Spec.Trigger != billingv1alpha1.ChargeTriggerBillingAccountActivation {
		t.Errorf("setup trigger = %v, want BillingAccountActivation", setup.Spec.Trigger)
	}
	if setup.Spec.Metric != nil || setup.Spec.PricingUnit != nil || len(setup.Spec.Rates) != 0 {
		t.Errorf("setup should not set metric/rates/pricingUnit: metric=%v unit=%v rates=%v",
			setup.Spec.Metric, setup.Spec.PricingUnit, setup.Spec.Rates)
	}

	platform := byName["compute-datumapis-com--platform-fee"]
	if platform == nil || platform.Spec == nil {
		t.Fatal("missing platform-fee ServicePricing")
	}
	if platform.Spec.ChargeType == nil || *platform.Spec.ChargeType != billingv1alpha1.ChargeTypeRecurring {
		t.Errorf("platform chargeType = %v, want Recurring", platform.Spec.ChargeType)
	}
	if platform.Spec.Interval == nil || *platform.Spec.Interval != billingv1alpha1.ChargeIntervalMonthly {
		t.Errorf("platform interval = %v, want monthly", platform.Spec.Interval)
	}
}

func TestUsesOrganizationDefaultQuotaGating(t *testing.T) {
	tests := []struct {
		name string
		sc   *servicesv1alpha1.ServiceConfiguration
		want bool
	}{
		{
			name: "nil billing",
			sc:   &servicesv1alpha1.ServiceConfiguration{},
			want: true,
		},
		{
			name: "empty gating",
			sc: &servicesv1alpha1.ServiceConfiguration{
				Spec: servicesv1alpha1.ServiceConfigurationSpec{
					Billing: &servicesv1alpha1.ServiceBillingConfig{},
				},
			},
			want: true,
		},
		{
			name: "organization default",
			sc: &servicesv1alpha1.ServiceConfiguration{
				Spec: servicesv1alpha1.ServiceConfigurationSpec{
					Billing: &servicesv1alpha1.ServiceBillingConfig{
						QuotaGating: servicesv1alpha1.QuotaGatingOrganizationDefault,
					},
				},
			},
			want: true,
		},
		{
			name: "billing entitlement",
			sc: &servicesv1alpha1.ServiceConfiguration{
				Spec: servicesv1alpha1.ServiceConfigurationSpec{
					Billing: &servicesv1alpha1.ServiceBillingConfig{
						QuotaGating: servicesv1alpha1.QuotaGatingBillingEntitlement,
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usesOrganizationDefaultQuotaGating(tt.sc); got != tt.want {
				t.Fatalf("usesOrganizationDefaultQuotaGating() = %v, want %v", got, tt.want)
			}
		})
	}
}
