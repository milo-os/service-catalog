// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
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

// TestValidateServiceConfigurationUpdate_AllowsChargeRemoval pins the
// relaxation that lets an apply omit a published charge. Removing one is still
// dangerous, and the check is expected back in a future milestone. Until then
// this asserts the relaxation is deliberate rather than accidental, since the
// original rejection shipped with no test at all.
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
