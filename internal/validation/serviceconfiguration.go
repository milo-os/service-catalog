// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	quotavalidation "go.miloapis.com/milo/pkg/quota/validation"
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
	allErrs = append(allErrs, validateNoCharges(sc)...)
	allErrs = append(allErrs, validateBillingDestinationRefs(sc, mrtNames, metricNames)...)
	allErrs = append(allErrs, validateQuotaLimitUniqueness(sc)...)
	allErrs = append(allErrs, validateQuotaRefs(sc, metricNames)...)
	allErrs = append(allErrs, validateProvisioning(sc)...)
	allErrs = append(allErrs, validateMigrateFromOffer(sc)...)
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
	allErrs = append(allErrs, validateMetricPhaseTransitions(oldSC, newSC)...)
	allErrs = append(allErrs, validateBillingDestinationRefs(newSC, mrtNames, metricNames)...)
	allErrs = append(allErrs, validateQuotaLimitUniqueness(newSC)...)
	allErrs = append(allErrs, validateQuotaRefs(newSC, metricNames)...)

	// Removing the controller's finalizer is an update, so re-validating a
	// terminating configuration would leave an already-invalid document
	// undeletable. A provisioning declaration goes invalid without being
	// edited, whenever the platform narrows the schema under it. Deleting
	// the document is the remedy.
	terminating := !newSC.DeletionTimestamp.IsZero()
	if !terminating {
		allErrs = append(allErrs, validateProvisioning(newSC)...)
		allErrs = append(allErrs, validateNoCharges(newSC)...)
	}
	allErrs = append(allErrs, validateMigrateFromOffer(newSC)...)
	if !isDryRun {
		allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, newSC)...)
		allErrs = append(allErrs, validateDefaultOffer(ctx, c, newSC)...)
		allErrs = append(allErrs, validateRetiredMetricsUnpriced(ctx, c, oldSC, newSC)...)
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
		allErrs = append(allErrs, validateMetricCostConditions(rulesPath.Index(i), rule)...)
	}
	return allErrs
}

const maxMetricCostConditionLength = 1024

// quotaConditionValidator applies the checks Milo runs on ClaimCreationPolicy
// trigger constraints, so a condition the catalog admits is one Milo accepts.
var quotaConditionValidator = func() *quotavalidation.CELValidator {
	v, err := quotavalidation.NewCELValidator()
	if err != nil {
		panic(fmt.Sprintf("build quota CEL validator: %v", err))
	}
	return v
}()

func validateMetricCostConditions(rulePath *field.Path, rule servicesv1alpha1.QuotaMetricRule) field.ErrorList {
	var allErrs field.ErrorList
	condPath := rulePath.Child("metricCostConditions")
	for _, k := range slices.Sorted(maps.Keys(rule.MetricCostConditions)) {
		expr := rule.MetricCostConditions[k]
		keyPath := condPath.Key(k)
		if _, ok := rule.MetricCosts[k]; !ok {
			allErrs = append(allErrs, field.Invalid(keyPath, k,
				"must name a metric that this rule charges in metricCosts"))
		}
		switch {
		case strings.TrimSpace(expr) == "":
			allErrs = append(allErrs, field.Required(keyPath, "a condition can't be empty"))
		case len(expr) > maxMetricCostConditionLength:
			allErrs = append(allErrs, field.TooLong(keyPath, "", maxMetricCostConditionLength))
		case strings.Contains(expr, "{{"):
			allErrs = append(allErrs, field.Invalid(keyPath, expr,
				"must be a plain CEL expression without {{ }} delimiters"))
		default:
			err := quotaConditionValidator.ValidateConstraints(
				[]quotav1alpha1.ConditionExpression{{Expression: expr}})
			if err != nil {
				allErrs = append(allErrs, field.Invalid(keyPath, expr,
					fmt.Sprintf("must be a CEL expression that returns a bool: %v", err)))
			}
		}
	}
	return allErrs
}

// validateNoCharges rejects spec.charges. Prices are standalone
// ServicePricing manifests (billing.miloapis.com) kept in the service's repo,
// one per environment, so a ServiceConfiguration carries no commercial terms.
func validateNoCharges(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	if len(sc.Spec.Charges) == 0 {
		return nil
	}
	return field.ErrorList{field.Forbidden(
		field.NewPath("spec", "charges"),
		"charges are retired; price this service with ServicePricing manifests (billing.miloapis.com/v1alpha1) in its repo instead",
	)}
}

// validateRetiredMetricsUnpriced rejects retiring a metric that a Usage
// ServicePricing still selects: a retired metric no longer accepts usage, so
// the pricing could never bill. Remove the pricing before retiring the
// metric.
func validateRetiredMetricsUnpriced(
	ctx context.Context,
	c client.Reader,
	oldSC, newSC *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	oldPhases := make(map[string]servicesv1alpha1.Phase, len(oldSC.Spec.Metrics))
	for _, m := range oldSC.Spec.Metrics {
		oldPhases[m.Name] = metricPhase(m)
	}
	retiring := make(map[string]int)
	for i, m := range newSC.Spec.Metrics {
		if metricPhase(m) == servicesv1alpha1.PhaseRetired && oldPhases[m.Name] != servicesv1alpha1.PhaseRetired {
			retiring[m.Name] = i
		}
	}
	if c == nil || len(retiring) == 0 {
		return nil
	}

	metricsPath := field.NewPath("spec", "metrics")
	var pricings billingv1alpha1.ServicePricingList
	if err := c.List(ctx, &pricings, client.InNamespace(billingv1alpha1.DefaultServicePricingNamespace)); err != nil {
		return field.ErrorList{field.InternalError(metricsPath, fmt.Errorf("failed to list ServicePricings: %w", err))}
	}

	var allErrs field.ErrorList
	for _, sp := range pricings.Items {
		if sp.Spec.ChargeType != billingv1alpha1.ChargeTypeUsage {
			continue
		}
		i, ok := retiring[sp.Spec.Metric]
		if !ok {
			continue
		}
		allErrs = append(allErrs, field.Invalid(
			metricsPath.Index(i).Child("phase"), string(servicesv1alpha1.PhaseRetired),
			fmt.Sprintf("ServicePricing %q still prices this metric, and a retired metric no longer accepts usage; remove the pricing before retiring the metric", sp.Name),
		))
	}
	return allErrs
}

// metricPhaseTransitionMessage is the error text for a metric phase change
// that skips ahead or moves back.
const metricPhaseTransitionMessage = "a metric only moves forward through its lifecycle, one step at a time (Published, then Deprecated, then Retired), and cannot move back or skip ahead"

// validateMetricPhaseTransitions enforces the forward-only lifecycle on each
// metric that exists in both versions. An unset phase counts as Published.
func validateMetricPhaseTransitions(oldSC, newSC *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	oldPhases := make(map[string]servicesv1alpha1.Phase, len(oldSC.Spec.Metrics))
	for _, m := range oldSC.Spec.Metrics {
		oldPhases[m.Name] = metricPhase(m)
	}
	var allErrs field.ErrorList
	for i, m := range newSC.Spec.Metrics {
		oldPhase, ok := oldPhases[m.Name]
		if !ok {
			continue
		}
		newPhase := metricPhase(m)
		if oldPhase != newPhase && phaseOrder(newPhase) != phaseOrder(oldPhase)+1 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec", "metrics").Index(i).Child("phase"), string(m.Phase), metricPhaseTransitionMessage,
			))
		}
	}
	return allErrs
}

// metricPhase is a metric's own lifecycle position; unset counts as
// Published.
func metricPhase(m servicesv1alpha1.MetricSpec) servicesv1alpha1.Phase {
	if m.Phase == "" {
		return servicesv1alpha1.PhasePublished
	}
	return m.Phase
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

// validateMigrateFromOffer checks intra-document rules for the one-shot
// default-Offer migration. The from-offer is not required to exist so a
// retired catalog entry can still be migrated off of.
func validateMigrateFromOffer(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	if sc.Spec.MigrateFromOffer == "" {
		return allErrs
	}
	fldPath := field.NewPath("spec", "migrateFromOffer")
	if sc.Spec.DefaultOffer == "" {
		allErrs = append(allErrs, field.Invalid(
			fldPath, sc.Spec.MigrateFromOffer,
			"defaultOffer must be set when migrateFromOffer is set",
		))
		return allErrs
	}
	if sc.Spec.MigrateFromOffer == sc.Spec.DefaultOffer {
		allErrs = append(allErrs, field.Invalid(
			fldPath, sc.Spec.MigrateFromOffer,
			"must differ from defaultOffer",
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
// core identity fields on meters and monitored resource types
// that were already present in the Published ServiceConfiguration. New
// entries are allowed. spec.defaultOffer and spec.migrateFromOffer are
// intentionally mutable on Published so staff can switch the platform
// default Offer and opt into migrating accounts still on the previous
// default without republishing the billing ServiceConfiguration.
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
		}
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
	}

	// Compare effective quotaGating even when old billing was nil so
	// introducing a billing block after publish cannot flip the mode.
	oldGating := effectiveQuotaGating(oldSC)
	newGating := effectiveQuotaGating(newSC)
	if oldGating != newGating {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "billing", "quotaGating"),
			"can't be changed once the configuration is published",
		))
	}

	return allErrs
}

// effectiveQuotaGating returns the QuotaGatingMode in force for sc.
// Empty / unset is treated as OrganizationDefault.
func effectiveQuotaGating(sc *servicesv1alpha1.ServiceConfiguration) servicesv1alpha1.QuotaGatingMode {
	if sc.Spec.Billing == nil || sc.Spec.Billing.QuotaGating == "" {
		return servicesv1alpha1.QuotaGatingOrganizationDefault
	}
	return sc.Spec.Billing.QuotaGating
}
