// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// ValidateServiceConfigurationCreate validates a ServiceConfiguration on
// creation. Runs intra-document consistency checks plus the Service
// lookup for name-prefix enforcement.
func ValidateServiceConfigurationCreate(
	ctx context.Context,
	c client.Reader,
	sc *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	mrtNames := collectMonitoredResourceTypeNames(sc)
	metricNames := collectMetricNames(sc)
	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(sc)...)
	allErrs = append(allErrs, validateMetricUniqueness(sc)...)
	allErrs = append(allErrs, validateBillingDestinationRefs(sc, mrtNames, metricNames)...)
	allErrs = append(allErrs, validateQuotaLimitUniqueness(sc)...)
	allErrs = append(allErrs, validateQuotaRefs(sc, metricNames)...)
	allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, sc)...)

	return allErrs
}

// ValidateServiceConfigurationUpdate validates a ServiceConfiguration on
// update. Runs the same consistency checks as create, plus phase
// transition and Published-phase immutability of core identity fields.
func ValidateServiceConfigurationUpdate(
	ctx context.Context,
	c client.Reader,
	oldSC, newSC *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	mrtNames := collectMonitoredResourceTypeNames(newSC)
	metricNames := collectMetricNames(newSC)
	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(newSC)...)
	allErrs = append(allErrs, validateMetricUniqueness(newSC)...)
	allErrs = append(allErrs, validateBillingDestinationRefs(newSC, mrtNames, metricNames)...)
	allErrs = append(allErrs, validateQuotaLimitUniqueness(newSC)...)
	allErrs = append(allErrs, validateQuotaRefs(newSC, metricNames)...)
	allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, newSC)...)
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
				"must match a spec.monitoredResourceTypes[].type",
			))
		}
		for j, m := range dest.Metrics {
			if _, ok := metricNames[m]; !ok {
				allErrs = append(allErrs, field.Invalid(
					path.Index(i).Child("metrics").Index(j), m,
					"must match a spec.metrics[].name",
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
				"must match a spec.metrics[].name",
			))
		}
	}
	rulesPath := field.NewPath("spec", "quota", "metricRules")
	for i, rule := range sc.Spec.Quota.MetricRules {
		for k := range rule.MetricCosts {
			if _, ok := metricNames[k]; !ok {
				allErrs = append(allErrs, field.Invalid(
					rulesPath.Index(i).Child("metricCosts"), k,
					"must match a spec.metrics[].name",
				))
			}
		}
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
				fmt.Sprintf("no Service with metadata.name %q exists", sc.Spec.ServiceRef.Name),
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
				fmt.Sprintf("must be prefixed with the referenced service %q (e.g. %q)",
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
				fmt.Sprintf("must be prefixed with the referenced service %q (e.g. %q)",
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
				fmt.Sprintf("monitored resource type %q cannot be removed or renamed once the ServiceConfiguration is Published", oldType),
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
				"gvk is immutable once the ServiceConfiguration is Published",
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
				fmt.Sprintf("cannot remove metric %q after publishing", name)))
			continue
		}
		for i, m := range newSC.Spec.Metrics {
			if m.Name != name {
				continue
			}
			if m.Kind != old.Kind {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("kind"),
					"immutable after publishing"))
			}
			if m.Unit != old.Unit {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("unit"),
					"immutable after publishing"))
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
					fmt.Sprintf("cannot remove quota limit %q after publishing", name)))
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
						"immutable after publishing"))
				}
				if l.Unit != old.Unit {
					allErrs = append(allErrs, field.Forbidden(quotaPath.Index(i).Child("unit"),
						"immutable after publishing"))
				}
				if l.ConsumerType != old.ConsumerType {
					allErrs = append(allErrs, field.Forbidden(quotaPath.Index(i).Child("consumerType"),
						"immutable after publishing"))
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
					fmt.Sprintf("cannot remove billing destination %q after publishing", mrt)))
				continue
			}
			newMetricSet := stringSet(newDest.Metrics)
			for _, m := range old.Metrics {
				if _, ok := newMetricSet[m]; !ok {
					allErrs = append(allErrs, field.Forbidden(billingPath,
						fmt.Sprintf("cannot remove metric %q from destination %q after publishing", m, mrt)))
				}
			}
		}
	}

	return allErrs
}
