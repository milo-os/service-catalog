// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newServiceConfigurationValidationScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = billingv1alpha1.AddToScheme(s)
	return s
}

func publishedBillingSC(defaultOffer string) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:   servicesv1alpha1.ServiceReference{Name: "billing-miloapis-com"},
			Phase:        servicesv1alpha1.PhasePublished,
			DefaultOffer: defaultOffer,
		},
	}
}

func gaOfferWithSnapshot(name string) *billingv1alpha1.Offer {
	return &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageGA,
			ChargeTypes: []billingv1alpha1.ChargeType{billingv1alpha1.ChargeTypeUsage},
			ServicePricings: []billingv1alpha1.ServicePricingSnapshot{
				{
					Name: "example-pricing",
					Spec: billingv1alpha1.ServicePricingSpec{
						ChargeType: billingv1alpha1.ChargeTypeUsage,
					},
				},
			},
		},
	}
}

func TestPublishedImmutability_AllowsDefaultOfferChange(t *testing.T) {
	oldSC := publishedBillingSC("payg-v1")
	newSC := publishedBillingSC("payg-v2")

	errs := validateServiceConfigurationPublishedImmutability(oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("expected defaultOffer change on Published to be allowed, got: %v", errs)
	}
}

func TestValidateDefaultOffer_AcceptsGAWithSnapshot(t *testing.T) {
	scheme := newServiceConfigurationValidationScheme()
	offer := gaOfferWithSnapshot("payg-v1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(offer).Build()

	sc := publishedBillingSC("payg-v1")
	errs := validateDefaultOffer(context.Background(), c, sc)
	if len(errs) != 0 {
		t.Fatalf("expected valid defaultOffer, got: %v", errs)
	}
}

func TestValidateDefaultOffer_RejectsDraftOffer(t *testing.T) {
	scheme := newServiceConfigurationValidationScheme()
	offer := &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{Name: "draft-v1"},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageDraft,
			ChargeTypes: []billingv1alpha1.ChargeType{billingv1alpha1.ChargeTypeUsage},
			ServicePricings: []billingv1alpha1.ServicePricingSnapshot{
				{Name: "example-pricing", Spec: billingv1alpha1.ServicePricingSpec{
					ChargeType: billingv1alpha1.ChargeTypeUsage,
				}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(offer).Build()

	sc := publishedBillingSC("draft-v1")
	errs := validateDefaultOffer(context.Background(), c, sc)
	if len(errs) == 0 {
		t.Fatal("expected Draft Offer to be rejected as defaultOffer")
	}
}

func TestValidateDefaultOffer_RejectsEmptySnapshot(t *testing.T) {
	scheme := newServiceConfigurationValidationScheme()
	offer := &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-v1"},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage:     billingv1alpha1.OfferLaunchStageGA,
			ChargeTypes:     []billingv1alpha1.ChargeType{billingv1alpha1.ChargeTypeUsage},
			ServicePricings: nil,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(offer).Build()

	sc := publishedBillingSC("empty-v1")
	errs := validateDefaultOffer(context.Background(), c, sc)
	if len(errs) == 0 {
		t.Fatal("expected GA Offer without snapshot to be rejected as defaultOffer")
	}
}

func TestValidateServiceConfigurationUpdate_AllowsDefaultOfferSwitch(t *testing.T) {
	scheme := newServiceConfigurationValidationScheme()
	oldOffer := gaOfferWithSnapshot("payg-v1")
	newOffer := gaOfferWithSnapshot("payg-v2")
	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: "billing.miloapis.com",
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, oldOffer, newOffer).Build()

	oldSC := publishedBillingSC("payg-v1")
	newSC := publishedBillingSC("payg-v2")

	errs := ValidateServiceConfigurationUpdate(context.Background(), c, oldSC, newSC, false)
	if len(errs) != 0 {
		t.Fatalf("expected Published defaultOffer switch to succeed, got: %v", errs)
	}
}

func TestValidateMigrateFromOffer(t *testing.T) {
	tests := []struct {
		name             string
		defaultOffer     string
		migrateFromOffer string
		wantErr          bool
	}{
		{
			name:             "unset is valid",
			defaultOffer:     "payg-v2",
			migrateFromOffer: "",
			wantErr:          false,
		},
		{
			name:             "differs from defaultOffer",
			defaultOffer:     "payg-v2",
			migrateFromOffer: "payg-v1",
			wantErr:          false,
		},
		{
			name:             "from-offer need not exist",
			defaultOffer:     "payg-v2",
			migrateFromOffer: "retired-v0",
			wantErr:          false,
		},
		{
			name:             "requires defaultOffer",
			defaultOffer:     "",
			migrateFromOffer: "payg-v1",
			wantErr:          true,
		},
		{
			name:             "must differ from defaultOffer",
			defaultOffer:     "payg-v1",
			migrateFromOffer: "payg-v1",
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := publishedBillingSC(tt.defaultOffer)
			sc.Spec.MigrateFromOffer = tt.migrateFromOffer
			errs := validateMigrateFromOffer(sc)
			if tt.wantErr && len(errs) == 0 {
				t.Fatal("expected error")
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
		})
	}
}

func TestPublishedImmutability_AllowsMigrateFromOfferChange(t *testing.T) {
	oldSC := publishedBillingSC("payg-v1")
	newSC := publishedBillingSC("payg-v2")
	newSC.Spec.MigrateFromOffer = "payg-v1"

	errs := validateServiceConfigurationPublishedImmutability(oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("expected migrateFromOffer change on Published to be allowed, got: %v", errs)
	}
}

// TestValidateServiceConfigurationUpdate_AllowsChargeRemoval: charges are
// retired, so an update that drops them is how a configuration moves off them.
func TestValidateServiceConfigurationUpdate_AllowsChargeRemoval(t *testing.T) {
	scheme := newServiceConfigurationValidationScheme()
	offer := gaOfferWithSnapshot("payg-v1")
	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: "billing.miloapis.com",
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, offer).Build()

	oldSC := publishedBillingSC("payg-v1")
	oldSC.Spec.Charges = []servicesv1alpha1.ServiceChargeSpec{{Name: "example.com/thing/bytes"}}

	newSC := publishedBillingSC("payg-v1")

	errs := ValidateServiceConfigurationUpdate(context.Background(), c, oldSC, newSC, false)
	for _, e := range errs {
		if e.Field == "spec.charges" {
			t.Fatalf("charge removal should be allowed, got: %v", e)
		}
	}
}

func TestValidateQuotaRefs_MetricCostConditions(t *testing.T) {
	const (
		ipClaims   = "ipam.miloapis.com/ipclaims"
		ipv4Claims = "ipam.miloapis.com/ipv4claims"
	)
	sc := func(conditions map[string]string) *servicesv1alpha1.ServiceConfiguration {
		return &servicesv1alpha1.ServiceConfiguration{
			Spec: servicesv1alpha1.ServiceConfigurationSpec{
				Quota: &servicesv1alpha1.ServiceQuotaConfig{
					MetricRules: []servicesv1alpha1.QuotaMetricRule{{
						Selector: servicesv1alpha1.QuotaMetricRuleSelector{
							APIGroup: "ipam.miloapis.com",
							Kind:     "IPClaim",
						},
						MetricCosts:          map[string]int64{ipClaims: 1, ipv4Claims: 1},
						MetricCostConditions: conditions,
					}},
				},
			},
		}
	}
	metrics := map[string]struct{}{ipClaims: {}, ipv4Claims: {}}

	cases := []struct {
		name       string
		conditions map[string]string
		wantField  string
	}{
		{name: "no conditions"},
		{
			name:       "condition on a charged metric",
			conditions: map[string]string{ipv4Claims: "trigger.spec.ipFamily == 'IPv4'"},
		},
		{
			name:       "condition on a metric the rule does not charge",
			conditions: map[string]string{"ipam.miloapis.com/ipv6claims": "true"},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv6claims]",
		},
		{
			name:       "template delimiters",
			conditions: map[string]string{ipv4Claims: "{{trigger.spec.ipFamily == 'IPv4'}}"},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
		{
			name:       "unparseable expression",
			conditions: map[string]string{ipv4Claims: "trigger.spec.ipFamily =="},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
		{
			name: "IPAM public IPv4 condition",
			conditions: map[string]string{ipv4Claims: "has(trigger.spec.ipFamily) && trigger.spec.ipFamily == 'IPv4' && " +
				"has(trigger.spec.addressScope) && trigger.spec.addressScope == 'Public'"},
		},
		{
			name:       "term Milo forbids",
			conditions: map[string]string{ipv4Claims: "trigger.spec.networkRef != ''"},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
		{
			name:       "non-bool result",
			conditions: map[string]string{ipv4Claims: "trigger.spec.ipFamily"},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
		{
			name:       "undeclared variable",
			conditions: map[string]string{ipv4Claims: "object.spec.ipFamily == 'IPv4'"},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
		{
			name:       "empty expression",
			conditions: map[string]string{ipv4Claims: " "},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
		{
			name:       "too long",
			conditions: map[string]string{ipv4Claims: "'" + strings.Repeat("a", 1024) + "' == ''"},
			wantField:  "spec.quota.metricRules[0].metricCostConditions[ipam.miloapis.com/ipv4claims]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateQuotaRefs(sc(tc.conditions), metrics)
			if tc.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
			}
			if errs[0].Field != tc.wantField {
				t.Errorf("error field = %q, want %q", errs[0].Field, tc.wantField)
			}
		})
	}
}

func TestValidateServiceConfigurationUpdate_AllowsAddingConditionalMetricWhenPublished(t *testing.T) {
	const (
		ipClaims   = "ipam.miloapis.com/ipclaims"
		ipv4Claims = "ipam.miloapis.com/ipv4claims"
	)
	rule := servicesv1alpha1.QuotaMetricRule{
		Selector:    servicesv1alpha1.QuotaMetricRuleSelector{APIGroup: "ipam.miloapis.com", Kind: "IPClaim"},
		MetricCosts: map[string]int64{ipClaims: 1},
	}
	oldSC := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "ipam-miloapis-com"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "ipam-miloapis-com"},
			Phase:      servicesv1alpha1.PhasePublished,
			Metrics:    []servicesv1alpha1.MetricSpec{{Name: ipClaims}},
			Quota:      &servicesv1alpha1.ServiceQuotaConfig{MetricRules: []servicesv1alpha1.QuotaMetricRule{rule}},
		},
	}
	newSC := oldSC.DeepCopy()
	newSC.Spec.Metrics = append(newSC.Spec.Metrics, servicesv1alpha1.MetricSpec{Name: ipv4Claims})
	newRule := &newSC.Spec.Quota.MetricRules[0]
	newRule.MetricCosts[ipv4Claims] = 1
	newRule.MetricCostConditions = map[string]string{ipv4Claims: "trigger.spec.ipFamily == 'IPv4'"}

	errs := validateServiceConfigurationPublishedImmutability(oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}
