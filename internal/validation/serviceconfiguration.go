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

	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(sc)...)
	allErrs = append(allErrs, validateMetricUniqueness(sc)...)
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

	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(newSC)...)
	allErrs = append(allErrs, validateMetricUniqueness(newSC)...)
	allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, newSC)...)

	allErrs = append(allErrs, ValidatePhaseTransition(
		oldSC.Spec.Phase, newSC.Spec.Phase,
		field.NewPath("spec", "phase"),
	)...)

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
	fldPath := field.NewPath("spec", "metrics")

	seen := make(map[string]int, len(sc.Spec.Metrics))
	for i, m := range sc.Spec.Metrics {
		if m.Name == "" {
			continue
		}
		if _, ok := seen[m.Name]; ok {
			allErrs = append(allErrs, field.Duplicate(
				fldPath.Index(i).Child("name"), m.Name,
			))
			continue
		}
		seen[m.Name] = i
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

	oldMetricsByName := make(map[string]servicesv1alpha1.MetricSpec, len(oldSC.Spec.Metrics))
	for _, m := range oldSC.Spec.Metrics {
		oldMetricsByName[m.Name] = m
	}
	newMetricsByName := make(map[string]struct{}, len(newSC.Spec.Metrics))
	for _, m := range newSC.Spec.Metrics {
		newMetricsByName[m.Name] = struct{}{}
	}
	metricsPath := field.NewPath("spec", "metrics")
	for oldName := range oldMetricsByName {
		if _, ok := newMetricsByName[oldName]; !ok {
			allErrs = append(allErrs, field.Forbidden(
				metricsPath,
				fmt.Sprintf("metric %q cannot be removed or renamed once the ServiceConfiguration is Published", oldName),
			))
		}
	}
	for i, newMetric := range newSC.Spec.Metrics {
		oldMetric, ok := oldMetricsByName[newMetric.Name]
		if !ok {
			continue
		}
		itemPath := metricsPath.Index(i)
		if oldMetric.Kind != newMetric.Kind {
			allErrs = append(allErrs, field.Forbidden(
				itemPath.Child("kind"),
				"kind is immutable once the ServiceConfiguration is Published",
			))
		}
		if oldMetric.Unit != newMetric.Unit {
			allErrs = append(allErrs, field.Forbidden(
				itemPath.Child("unit"),
				"unit is immutable once the ServiceConfiguration is Published",
			))
		}
	}

	return allErrs
}
