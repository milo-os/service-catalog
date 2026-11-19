// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const testMetric = "compute.datumapis.com/instance/memory-seconds"

// computeSC is a published configuration with one metric in phase.
func computeSC(phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{{
				Name: testMetric, Kind: servicesv1alpha1.MetricKindCumulative, Unit: "By.s", Phase: phase,
			}},
		},
	}
}

func hasError(errs field.ErrorList, fieldPath, contains string) bool {
	for _, e := range errs {
		if e.Field == fieldPath && strings.Contains(e.Error(), contains) {
			return true
		}
	}
	return false
}

// TestValidateMetricPhaseTransitions: a metric is retired one step at a
// time and never brought back. Unset counts as Published.
func TestValidateMetricPhaseTransitions(t *testing.T) {
	const (
		unset      = servicesv1alpha1.Phase("")
		published  = servicesv1alpha1.PhasePublished
		deprecated = servicesv1alpha1.PhaseDeprecated
		retired    = servicesv1alpha1.PhaseRetired
	)
	tests := []struct {
		from, to servicesv1alpha1.Phase
		ok       bool
	}{
		{unset, unset, true},
		{unset, published, true},
		{unset, deprecated, true},
		{published, deprecated, true},
		{deprecated, retired, true},
		{retired, retired, true},
		{unset, retired, false},
		{published, retired, false},
		{deprecated, published, false},
		{deprecated, unset, false},
		{retired, deprecated, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			errs := validateMetricPhaseTransitions(computeSC(tt.from), computeSC(tt.to))
			if got := len(errs) == 0; got != tt.ok {
				t.Errorf("%q -> %q: errs = %v, want ok=%v", tt.from, tt.to, errs, tt.ok)
			}
		})
	}

	// A metric added in this update can start in any phase.
	oldSC := computeSC(unset)
	oldSC.Spec.Metrics = nil
	if errs := validateMetricPhaseTransitions(oldSC, computeSC(retired)); len(errs) != 0 {
		t.Errorf("new metric: errs = %v, want none", errs)
	}
}

func memoryPricing(name string, chargeType billingv1alpha1.ChargeType) *billingv1alpha1.ServicePricing {
	return &billingv1alpha1.ServicePricing{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: billingv1alpha1.DefaultServicePricingNamespace},
		Spec: billingv1alpha1.ServicePricingSpec{
			ChargeType:  chargeType,
			ServiceRef:  "compute.datumapis.com",
			Currency:    "USD",
			Metric:      testMetric,
			PricingUnit: "gib-second",
			Rates:       []billingv1alpha1.PricingRate{{Flat: "0.0000045"}},
		},
	}
}

// TestValidateRetiredMetricsUnpriced: a retired metric accepts no usage, so
// retiring one a ServicePricing still selects is rejected. Deprecating it is
// fine, and so is retiring a metric nothing prices.
func TestValidateRetiredMetricsUnpriced(t *testing.T) {
	const metricPhasePath = "spec.metrics[0].phase"
	priced := fake.NewClientBuilder().WithScheme(newServiceConfigurationValidationScheme()).
		WithObjects(memoryPricing("compute-memory-v1", billingv1alpha1.ChargeTypeUsage)).Build()
	unpriced := fake.NewClientBuilder().WithScheme(newServiceConfigurationValidationScheme()).Build()
	deprecated, retired := servicesv1alpha1.PhaseDeprecated, servicesv1alpha1.PhaseRetired

	if errs := validateRetiredMetricsUnpriced(context.Background(), priced, computeSC(""), computeSC(deprecated)); len(errs) != 0 {
		t.Errorf("deprecating a priced metric: errs = %v, want none", errs)
	}
	errs := validateRetiredMetricsUnpriced(context.Background(), priced, computeSC(deprecated), computeSC(retired))
	if !hasError(errs, metricPhasePath, "compute-memory-v1") {
		t.Errorf("retiring a priced metric: errs = %v, want an error naming the pricing", errs)
	}
	if errs := validateRetiredMetricsUnpriced(context.Background(), unpriced, computeSC(deprecated), computeSC(retired)); len(errs) != 0 {
		t.Errorf("retiring an unpriced metric: errs = %v, want none", errs)
	}
	if errs := validateRetiredMetricsUnpriced(context.Background(), priced, computeSC(retired), computeSC(retired)); len(errs) != 0 {
		t.Errorf("metric already retired: errs = %v, want none", errs)
	}
}

// TestValidateNoCharges: charges are retired; a configuration without them
// passes.
func TestValidateNoCharges(t *testing.T) {
	if errs := validateNoCharges(computeSC("")); len(errs) != 0 {
		t.Errorf("no charges: errs = %v, want none", errs)
	}
	sc := computeSC("")
	sc.Spec.Charges = []servicesv1alpha1.ServiceChargeSpec{{
		Name:       "compute.datumapis.com/instance/memory-gib-seconds",
		ChargeType: servicesv1alpha1.ServiceChargeTypeUsage,
	}}
	if errs := validateNoCharges(sc); !hasError(errs, "spec.charges", "ServicePricing") {
		t.Errorf("with charges: errs = %v, want a spec.charges error", errs)
	}
}

// TestPublishedImmutability_AllowsMetricPhaseChange: retiring a metric is a
// change to a published configuration; its identity (kind, unit) stays.
func TestPublishedImmutability_AllowsMetricPhaseChange(t *testing.T) {
	errs := validateServiceConfigurationPublishedImmutability(
		computeSC(""), computeSC(servicesv1alpha1.PhaseDeprecated))
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
}
