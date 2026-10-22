// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const testMetric = "compute.datumapis.com/instance/memory-seconds"

// computeSC is a published configuration with one metric (in phase) and,
// when charge is set, one Usage charge on it.
func computeSC(phase servicesv1alpha1.Phase, charge *servicesv1alpha1.UsageChargeOptions) *servicesv1alpha1.ServiceConfiguration {
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
			Metrics: []servicesv1alpha1.MetricSpec{{
				Name: testMetric, Kind: servicesv1alpha1.MetricKindCumulative, Unit: "By.s", Phase: phase,
			}},
		},
	}
	if charge != nil {
		sc.Spec.Charges = []servicesv1alpha1.ServiceChargeSpec{{
			Name:       "compute.datumapis.com/instance/memory-gib-seconds",
			ChargeType: servicesv1alpha1.ServiceChargeTypeUsage,
			Usage:      charge,
		}}
	}
	return sc
}

func memoryCharge(uc *servicesv1alpha1.UnitConversion) *servicesv1alpha1.UsageChargeOptions {
	return &servicesv1alpha1.UsageChargeOptions{
		MetricRef:      testMetric,
		PricingUnit:    "gib-second",
		Rates:          []servicesv1alpha1.PricingRateEntry{{Flat: "0.0000045"}},
		UnitConversion: uc,
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
			errs := validateMetricPhaseTransitions(computeSC(tt.from, nil), computeSC(tt.to, nil))
			if got := len(errs) == 0; got != tt.ok {
				t.Errorf("%q -> %q: errs = %v, want ok=%v", tt.from, tt.to, errs, tt.ok)
			}
		})
	}

	// A metric added in this update can start in any phase.
	oldSC := computeSC(unset, nil)
	oldSC.Spec.Metrics = nil
	if errs := validateMetricPhaseTransitions(oldSC, computeSC(retired, nil)); len(errs) != 0 {
		t.Errorf("new metric: errs = %v, want none", errs)
	}
}

// TestValidateChargesPriceLiveMetrics: a retired metric accepts no usage,
// so a charge on it could never bill. Deprecated metrics are still priced.
func TestValidateChargesPriceLiveMetrics(t *testing.T) {
	if errs := validateChargesPriceLiveMetrics(computeSC(servicesv1alpha1.PhaseDeprecated, memoryCharge(nil))); len(errs) != 0 {
		t.Errorf("charge on a deprecated metric: errs = %v, want none", errs)
	}
	errs := validateChargesPriceLiveMetrics(computeSC(servicesv1alpha1.PhaseRetired, memoryCharge(nil)))
	if !hasError(errs, "spec.charges[0].usage.metricRef", "retired metric") {
		t.Errorf("charge on a retired metric: errs = %v, want a metricRef error", errs)
	}
}

func TestValidateUnitConversion(t *testing.T) {
	const path = "spec.charges[0].usage.unitConversion"
	tests := []struct {
		name      string
		uc        *servicesv1alpha1.UnitConversion
		wantField string
	}{
		{"none", nil, ""},
		{"bytes per GiB", &servicesv1alpha1.UnitConversion{MeterUnitsPerPricingUnit: "1073741824", Description: "bytes to GiB"}, ""},
		{"fraction", &servicesv1alpha1.UnitConversion{MeterUnitsPerPricingUnit: "0.001", Description: "x"}, ""},
		{"zero", &servicesv1alpha1.UnitConversion{MeterUnitsPerPricingUnit: "0", Description: "x"}, path + ".meterUnitsPerPricingUnit"},
		{"negative", &servicesv1alpha1.UnitConversion{MeterUnitsPerPricingUnit: "-5", Description: "x"}, path + ".meterUnitsPerPricingUnit"},
		{"no description", &servicesv1alpha1.UnitConversion{MeterUnitsPerPricingUnit: "1000", Description: " "}, path + ".description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := computeSC("", memoryCharge(tt.uc))
			errs := validateCharges(sc, collectMetricNames(sc))
			switch {
			case tt.wantField == "" && len(errs) != 0:
				t.Errorf("errs = %v, want none", errs)
			case tt.wantField != "" && !hasError(errs, tt.wantField, ""):
				t.Errorf("errs = %v, want an error on %s", errs, tt.wantField)
			}
		})
	}
}

// TestPublishedImmutability_AllowsMetricPhaseChange: retiring a metric is a
// change to a published configuration; its identity (kind, unit) stays.
func TestPublishedImmutability_AllowsMetricPhaseChange(t *testing.T) {
	errs := validateServiceConfigurationPublishedImmutability(
		computeSC("", nil), computeSC(servicesv1alpha1.PhaseDeprecated, nil))
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
}
