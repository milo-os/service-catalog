// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ValidateServiceConfigurationCreate validates a ServiceConfiguration on
// creation. Runs intra-document consistency checks plus the Service
// lookup for name-prefix enforcement.
func ValidateServiceConfigurationCreate(
	ctx context.Context,
	c client.Reader,
	sc *servicesv1alpha1.ServiceConfiguration,
	isDryRun bool,
) field.ErrorList {
	var allErrs field.ErrorList

	mrtNames := collectMonitoredResourceTypeNames(sc)
	metricNames := collectMetricNames(sc)
	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(sc)...)
	allErrs = append(allErrs, validateMetricUniqueness(sc)...)
	allErrs = append(allErrs, validateMetricPricing(sc)...)
	allErrs = append(allErrs, validateCharges(sc)...)
	allErrs = append(allErrs, validateBillingDestinationRefs(sc, mrtNames, metricNames)...)
	allErrs = append(allErrs, validateQuotaLimitUniqueness(sc)...)
	allErrs = append(allErrs, validateQuotaRefs(sc, metricNames)...)
	if !isDryRun {
		allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, sc)...)
		allErrs = append(allErrs, validateDefaultOffer(ctx, c, sc)...)
	}

	return allErrs
}

// ValidateServiceConfigurationUpdate validates a ServiceConfiguration on
// update. Runs the same consistency checks as create, plus phase
// transition and Published-phase immutability of core identity fields.
func ValidateServiceConfigurationUpdate(
	ctx context.Context,
	c client.Reader,
	oldSC, newSC *servicesv1alpha1.ServiceConfiguration,
	isDryRun bool,
) field.ErrorList {
	var allErrs field.ErrorList

	mrtNames := collectMonitoredResourceTypeNames(newSC)
	metricNames := collectMetricNames(newSC)
	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(newSC)...)
	allErrs = append(allErrs, validateMetricUniqueness(newSC)...)
	allErrs = append(allErrs, validateMetricPricing(newSC)...)
	allErrs = append(allErrs, validateCharges(newSC)...)
	allErrs = append(allErrs, validateBillingDestinationRefs(newSC, mrtNames, metricNames)...)
	allErrs = append(allErrs, validateQuotaLimitUniqueness(newSC)...)
	allErrs = append(allErrs, validateQuotaRefs(newSC, metricNames)...)
	if !isDryRun {
		allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, newSC)...)
		allErrs = append(allErrs, validateDefaultOffer(ctx, c, newSC)...)
	}
	allErrs = append(allErrs, ValidatePhaseTransition(oldSC.Spec.Phase, newSC.Spec.Phase, field.NewPath("spec", "phase"))...)
	if oldSC.Spec.Phase == servicesv1alpha1.PhasePublished {
		allErrs = append(allErrs, validateServiceConfigurationPublishedImmutability(oldSC, newSC)...)
	}

	return allErrs
}

func validateMonitoredResourceTypeUniqueness(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "monitoredResourceTypes")

	seen := make(map[string]int, len(sc.Spec.MonitoredResourceTypes))
	for i, mrt := range sc.Spec.MonitoredResourceTypes {
		if mrt.Type == "" {
			continue
		}
		if _, ok := seen[mrt.Type]; ok {
			allErrs = append(allErrs, field.Duplicate(
				fldPath.Index(i).Child("type"), mrt.Type,
			))
			continue
		}
		seen[mrt.Type] = i
	}
	return allErrs
}

func validateMetricUniqueness(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	seen := make(map[string]struct{}, len(sc.Spec.Metrics))
	path := field.NewPath("spec", "metrics")
	for i, m := range sc.Spec.Metrics {
		if _, dup := seen[m.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(path.Index(i).Child("name"), m.Name))
		}
		seen[m.Name] = struct{}{}
	}
	return allErrs
}

func stringSet(items []string) map[string]struct{} {
	s := make(map[string]struct{}, len(items))
	for _, v := range items {
		s[v] = struct{}{}
	}
	return s
}

func collectMetricNames(sc *servicesv1alpha1.ServiceConfiguration) map[string]struct{} {
	names := make(map[string]struct{}, len(sc.Spec.Metrics))
	for _, m := range sc.Spec.Metrics {
		names[m.Name] = struct{}{}
	}
	return names
}

func collectMonitoredResourceTypeNames(sc *servicesv1alpha1.ServiceConfiguration) map[string]struct{} {
	names := make(map[string]struct{}, len(sc.Spec.MonitoredResourceTypes))
	for _, mrt := range sc.Spec.MonitoredResourceTypes {
		names[mrt.Type] = struct{}{}
	}
	return names
}

func validateBillingDestinationRefs(
	sc *servicesv1alpha1.ServiceConfiguration,
	mrtNames, metricNames map[string]struct{},
) field.ErrorList {
	var allErrs field.ErrorList
	if sc.Spec.Billing == nil {
		return nil
	}
	path := field.NewPath("spec", "billing", "consumerDestinations")
	for i, dest := range sc.Spec.Billing.ConsumerDestinations {
		if _, ok := mrtNames[dest.MonitoredResourceType]; !ok {
			allErrs = append(allErrs, field.Invalid(
				path.Index(i).Child("monitoredResourceType"),
				dest.MonitoredResourceType,
				"must name a monitored resource type that this configuration defines",
			))
		}
		for j, m := range dest.Metrics {
			if _, ok := metricNames[m]; !ok {
				allErrs = append(allErrs, field.Invalid(
					path.Index(i).Child("metrics").Index(j), m,
					"must name a metric that this configuration defines",
				))
			}
		}
	}
	return allErrs
}

func validateQuotaLimitUniqueness(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	if sc.Spec.Quota == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(sc.Spec.Quota.Limits))
	path := field.NewPath("spec", "quota", "limits")
	for i, l := range sc.Spec.Quota.Limits {
		if _, dup := seen[l.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(path.Index(i).Child("name"), l.Name))
		}
		seen[l.Name] = struct{}{}
	}
	return allErrs
}

func validateQuotaRefs(
	sc *servicesv1alpha1.ServiceConfiguration,
	metricNames map[string]struct{},
) field.ErrorList {
	var allErrs field.ErrorList
	if sc.Spec.Quota == nil {
		return nil
	}
	limitsPath := field.NewPath("spec", "quota", "limits")
	for i, l := range sc.Spec.Quota.Limits {
		if _, ok := metricNames[l.Metric]; !ok {
			allErrs = append(allErrs, field.Invalid(
				limitsPath.Index(i).Child("metric"), l.Metric,
				"must name a metric that this configuration defines",
			))
		}
	}
	rulesPath := field.NewPath("spec", "quota", "metricRules")
	for i, rule := range sc.Spec.Quota.MetricRules {
		for k := range rule.MetricCosts {
			if _, ok := metricNames[k]; !ok {
				allErrs = append(allErrs, field.Invalid(
					rulesPath.Index(i).Child("metricCosts"), k,
					"must name a metric that this configuration defines",
				))
			}
		}
	}
	return allErrs
}

// validateMetricPricing validates metrics[].pricing: USD currency, flat XOR
// tiered rates, and tier band shape.
func validateMetricPricing(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	path := field.NewPath("spec", "metrics")
	for i, m := range sc.Spec.Metrics {
		if m.Pricing == nil {
			continue
		}
		pricingPath := path.Index(i).Child("pricing")
		allErrs = append(allErrs, validateMetricPricingEntry(m.Pricing, pricingPath)...)
	}
	return allErrs
}

func validateMetricPricingEntry(pricing *servicesv1alpha1.MetricPricing, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if pricing.Currency != "" && pricing.Currency != "USD" {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("currency"),
			pricing.Currency,
			"currency must be USD",
		))
	}
	if pricing.PricingUnit == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("pricingUnit"), "pricingUnit is required"))
	}
	if len(pricing.Rates) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("rates"), "rates is required"))
		return allErrs
	}
	allErrs = append(allErrs, validatePricingRateEntries(pricing.Rates, fldPath.Child("rates"))...)
	return allErrs
}

func validatePricingRateEntries(rates []servicesv1alpha1.PricingRateEntry, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for i := range rates {
		allErrs = append(allErrs, validatePricingRateEntry(&rates[i], fldPath.Index(i))...)
	}
	return allErrs
}

func validatePricingRateEntry(rate *servicesv1alpha1.PricingRateEntry, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	hasFlat := rate.Flat != ""
	hasTiered := len(rate.Tiered) > 0

	switch {
	case hasFlat && hasTiered:
		allErrs = append(allErrs, field.Invalid(
			fldPath,
			rate,
			"exactly one of flat or tiered must be set",
		))
	case !hasFlat && !hasTiered:
		allErrs = append(allErrs, field.Required(
			fldPath,
			"exactly one of flat or tiered must be set",
		))
	case hasTiered:
		allErrs = append(allErrs, validatePricingTiers(rate.Tiered, fldPath.Child("tiered"))...)
	}

	return allErrs
}

func validatePricingTiers(tiers []servicesv1alpha1.PricingTierBand, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(tiers) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "tiered must contain at least one band"))
		return allErrs
	}

	last := len(tiers) - 1
	for i, band := range tiers {
		bandPath := fldPath.Index(i)
		if band.Rate == "" {
			allErrs = append(allErrs, field.Required(bandPath.Child("rate"), "rate is required"))
		}
		if i != last && band.UpTo == "" {
			allErrs = append(allErrs, field.Required(
				bandPath.Child("upTo"),
				fmt.Sprintf("upTo is required on all but the last tiered band (index %d)", i),
			))
		}
	}

	return allErrs
}

// validateCharges validates charges[]: unique names, required fields per
// charge type, and USD currency.
func validateCharges(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	path := field.NewPath("spec", "charges")
	seen := make(map[string]struct{}, len(sc.Spec.Charges))

	for i, charge := range sc.Spec.Charges {
		itemPath := path.Index(i)
		if charge.Name != "" {
			if _, dup := seen[charge.Name]; dup {
				allErrs = append(allErrs, field.Duplicate(itemPath.Child("name"), charge.Name))
			}
			seen[charge.Name] = struct{}{}
		}
		allErrs = append(allErrs, validateChargeEntry(&charge, itemPath)...)
	}
	return allErrs
}

func validateChargeEntry(charge *servicesv1alpha1.ServiceChargeSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if charge.Currency != "" && charge.Currency != "USD" {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("currency"),
			charge.Currency,
			"currency must be USD",
		))
	}
	if charge.Amount == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("amount"), "amount is required"))
	}

	switch charge.ChargeType {
	case servicesv1alpha1.ServiceChargeTypeOneTime:
		if charge.Trigger == "" {
			allErrs = append(allErrs, field.Required(
				fldPath.Child("trigger"),
				"trigger is required when chargeType is OneTime",
			))
		}
	case servicesv1alpha1.ServiceChargeTypeRecurring:
		if charge.Interval == "" {
			allErrs = append(allErrs, field.Required(
				fldPath.Child("interval"),
				"interval is required when chargeType is Recurring",
			))
		}
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("chargeType"), "chargeType is required"))
	default:
		allErrs = append(allErrs, field.NotSupported(
			fldPath.Child("chargeType"),
			charge.ChargeType,
			[]string{
				string(servicesv1alpha1.ServiceChargeTypeOneTime),
				string(servicesv1alpha1.ServiceChargeTypeRecurring),
			},
		))
	}

	return allErrs
}

// validateDefaultOffer ensures that when defaultOffer is set, the named
// Offer exists and is assignable (GA with a non-empty servicePricings
// snapshot). OfferIsAssignable lives in billing/internal, so the check is
// inlined here against the public Offer API.
func validateDefaultOffer(
	ctx context.Context,
	c client.Reader,
	sc *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "defaultOffer")

	if sc.Spec.DefaultOffer == "" || c == nil {
		return allErrs
	}

	var offer billingv1alpha1.Offer
	if err := c.Get(ctx, types.NamespacedName{Name: sc.Spec.DefaultOffer}, &offer); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				fldPath, sc.Spec.DefaultOffer,
				fmt.Sprintf("offer %q does not exist", sc.Spec.DefaultOffer),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to load referenced Offer: %w", err)))
		return allErrs
	}

	if offer.Spec.LaunchStage != billingv1alpha1.OfferLaunchStageGA {
		allErrs = append(allErrs, field.Invalid(
			fldPath, sc.Spec.DefaultOffer,
			fmt.Sprintf("offer %q must have launchStage GA (got %q)", sc.Spec.DefaultOffer, offer.Spec.LaunchStage),
		))
	}
	if len(offer.Spec.ServicePricings) == 0 {
		allErrs = append(allErrs, field.Invalid(
			fldPath, sc.Spec.DefaultOffer,
			fmt.Sprintf("offer %q must have a non-empty servicePricings snapshot", sc.Spec.DefaultOffer),
		))
	}

	return allErrs
}

// validateServiceConfigurationNamePrefixes resolves the referenced
// Service and enforces that every meter.name and
// monitoredResourceType.type is prefixed by the Service's canonical
// spec.serviceName.
func validateServiceConfigurationNamePrefixes(
	ctx context.Context,
	c client.Reader,
	sc *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList
	serviceRefPath := field.NewPath("spec", "serviceRef", "name")

	if c == nil || sc.Spec.ServiceRef.Name == "" {
		return allErrs
	}

	var svc servicesv1alpha1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				serviceRefPath, sc.Spec.ServiceRef.Name,
				fmt.Sprintf("the service %q this configuration refers to does not exist", sc.Spec.ServiceRef.Name),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(serviceRefPath,
			fmt.Errorf("failed to load referenced Service: %w", err)))
		return allErrs
	}

	canonical := svc.Spec.ServiceName
	if canonical == "" {
		return allErrs
	}
	prefix := canonical + "/"

	mrtsPath := field.NewPath("spec", "monitoredResourceTypes")
	for i, mrt := range sc.Spec.MonitoredResourceTypes {
		if mrt.Type == "" {
			continue
		}
		if !strings.HasPrefix(mrt.Type, prefix) || strings.TrimPrefix(mrt.Type, prefix) == "" {
			allErrs = append(allErrs, field.Invalid(
				mrtsPath.Index(i).Child("type"), mrt.Type,
				fmt.Sprintf("must start with the service's name %q so it stays unique to this service (for example, %q)",
					prefix, prefix+"ExampleKind"),
			))
		}
	}

	metricsPath := field.NewPath("spec", "metrics")
	for i, m := range sc.Spec.Metrics {
		if m.Name == "" {
			continue
		}
		if !strings.HasPrefix(m.Name, prefix) || strings.TrimPrefix(m.Name, prefix) == "" {
			allErrs = append(allErrs, field.Invalid(
				metricsPath.Index(i).Child("name"), m.Name,
				fmt.Sprintf("must start with the service's name %q so it stays unique to this service (for example, %q)",
					prefix, prefix+"example-metric"),
			))
		}
	}
	return allErrs
}

// validateServiceConfigurationPublishedImmutability rejects changes to
// core identity fields on meters and monitored resource types that were
// already present in the Published ServiceConfiguration. New entries
// are allowed; entries removed while Published fall through to the
// phase/removal semantics handled elsewhere.
func validateServiceConfigurationPublishedImmutability(
	oldSC, newSC *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	oldMRTsByType := make(map[string]servicesv1alpha1.MonitoredResourceTypeSpec, len(oldSC.Spec.MonitoredResourceTypes))
	for _, mrt := range oldSC.Spec.MonitoredResourceTypes {
		oldMRTsByType[mrt.Type] = mrt
	}
	newMRTsByType := make(map[string]struct{}, len(newSC.Spec.MonitoredResourceTypes))
	for _, mrt := range newSC.Spec.MonitoredResourceTypes {
		newMRTsByType[mrt.Type] = struct{}{}
	}
	mrtsPath := field.NewPath("spec", "monitoredResourceTypes")
	for oldType := range oldMRTsByType {
		if _, ok := newMRTsByType[oldType]; !ok {
			allErrs = append(allErrs, field.Forbidden(
				mrtsPath,
				fmt.Sprintf("the monitored resource type %q can't be removed or renamed once the configuration is published", oldType),
			))
		}
	}
	for i, newMRT := range newSC.Spec.MonitoredResourceTypes {
		oldMRT, ok := oldMRTsByType[newMRT.Type]
		if !ok {
			continue
		}
		itemPath := mrtsPath.Index(i)
		if oldMRT.GVK != newMRT.GVK {
			allErrs = append(allErrs, field.Forbidden(
				itemPath.Child("gvk"),
				"the resource type can't be changed once the configuration is published",
			))
		}
	}

	oldMetrics := make(map[string]servicesv1alpha1.MetricSpec, len(oldSC.Spec.Metrics))
	for _, m := range oldSC.Spec.Metrics {
		oldMetrics[m.Name] = m
	}
	newMetricSet := make(map[string]struct{}, len(newSC.Spec.Metrics))
	for _, m := range newSC.Spec.Metrics {
		newMetricSet[m.Name] = struct{}{}
	}
	metricsPath := field.NewPath("spec", "metrics")
	for name, old := range oldMetrics {
		if _, exists := newMetricSet[name]; !exists {
			allErrs = append(allErrs, field.Forbidden(metricsPath,
				fmt.Sprintf("the metric %q can't be removed once the configuration is published", name)))
			continue
		}
		for i, m := range newSC.Spec.Metrics {
			if m.Name != name {
				continue
			}
			if m.Kind != old.Kind {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("kind"),
					"can't be changed once the configuration is published"))
			}
			if m.Unit != old.Unit {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("unit"),
					"can't be changed once the configuration is published"))
			}
			if !apiequality.Semantic.DeepEqual(old.Pricing, m.Pricing) {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("pricing"),
					"can't be changed once the configuration is published"))
			}
		}
	}

	oldCharges := make(map[string]servicesv1alpha1.ServiceChargeSpec, len(oldSC.Spec.Charges))
	for _, c := range oldSC.Spec.Charges {
		oldCharges[c.Name] = c
	}
	newChargeSet := make(map[string]struct{}, len(newSC.Spec.Charges))
	for _, c := range newSC.Spec.Charges {
		newChargeSet[c.Name] = struct{}{}
	}
	chargesPath := field.NewPath("spec", "charges")
	for name, old := range oldCharges {
		if _, exists := newChargeSet[name]; !exists {
			allErrs = append(allErrs, field.Forbidden(chargesPath,
				fmt.Sprintf("the charge %q can't be removed once the configuration is published", name)))
			continue
		}
		for i, c := range newSC.Spec.Charges {
			if c.Name != name {
				continue
			}
			if !apiequality.Semantic.DeepEqual(old, c) {
				allErrs = append(allErrs, field.Forbidden(chargesPath.Index(i),
					"can't be changed once the configuration is published"))
			}
		}
	}

	if oldSC.Spec.DefaultOffer != newSC.Spec.DefaultOffer {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "defaultOffer"),
			"can't be changed once the configuration is published",
		))
	}

	if oldSC.Spec.Quota != nil {
		oldLimits := make(map[string]servicesv1alpha1.QuotaLimitSpec, len(oldSC.Spec.Quota.Limits))
		for _, l := range oldSC.Spec.Quota.Limits {
			oldLimits[l.Name] = l
		}
		newLimitSet := make(map[string]struct{})
		if newSC.Spec.Quota != nil {
			for _, l := range newSC.Spec.Quota.Limits {
				newLimitSet[l.Name] = struct{}{}
			}
		}
		quotaPath := field.NewPath("spec", "quota", "limits")
		for name, old := range oldLimits {
			if _, exists := newLimitSet[name]; !exists {
				allErrs = append(allErrs, field.Forbidden(quotaPath,
					fmt.Sprintf("the quota limit %q can't be removed once the configuration is published", name)))
				continue
			}
			if newSC.Spec.Quota == nil {
				continue
			}
			for i, l := range newSC.Spec.Quota.Limits {
				if l.Name != name {
					continue
				}
				if l.Metric != old.Metric {
					allErrs = append(allErrs, field.Forbidden(quotaPath.Index(i).Child("metric"),
						"can't be changed once the configuration is published"))
				}
				if l.Unit != old.Unit {
					allErrs = append(allErrs, field.Forbidden(quotaPath.Index(i).Child("unit"),
						"can't be changed once the configuration is published"))
				}
				if l.ConsumerType != old.ConsumerType {
					allErrs = append(allErrs, field.Forbidden(quotaPath.Index(i).Child("consumerType"),
						"can't be changed once the configuration is published"))
				}
			}
		}
	}

	if oldSC.Spec.Billing != nil {
		oldDests := make(map[string]servicesv1alpha1.BillingConsumerDestination)
		for _, d := range oldSC.Spec.Billing.ConsumerDestinations {
			oldDests[d.MonitoredResourceType] = d
		}
		newDests := make(map[string]servicesv1alpha1.BillingConsumerDestination)
		if newSC.Spec.Billing != nil {
			for _, d := range newSC.Spec.Billing.ConsumerDestinations {
				newDests[d.MonitoredResourceType] = d
			}
		}
		billingPath := field.NewPath("spec", "billing", "consumerDestinations")
		for mrt, old := range oldDests {
			newDest, exists := newDests[mrt]
			if !exists {
				allErrs = append(allErrs, field.Forbidden(billingPath,
					fmt.Sprintf("the billing destination %q can't be removed once the configuration is published", mrt)))
				continue
			}
			newMetricSet := stringSet(newDest.Metrics)
			for _, m := range old.Metrics {
				if _, ok := newMetricSet[m]; !ok {
					allErrs = append(allErrs, field.Forbidden(billingPath,
						fmt.Sprintf("the metric %q can't be removed from billing destination %q once the configuration is published", m, mrt)))
				}
			}
		}

		oldGating := oldSC.Spec.Billing.QuotaGating
		newGating := servicesv1alpha1.QuotaGatingMode("")
		if newSC.Spec.Billing != nil {
			newGating = newSC.Spec.Billing.QuotaGating
		}
		if oldGating != newGating {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "billing", "quotaGating"),
				"can't be changed once the configuration is published",
			))
		}
	}

	return allErrs
}
