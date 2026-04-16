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
	c client.Client,
	sc *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	mrtNames := collectMonitoredResourceTypeNames(sc)
	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(sc)...)
	allErrs = append(allErrs, validateMeterUniqueness(sc)...)
	allErrs = append(allErrs, validateMeterMonitoredResourceTypeRefs(sc, mrtNames)...)
	allErrs = append(allErrs, validateServiceConfigurationNamePrefixes(ctx, c, sc)...)

	return allErrs
}

// ValidateServiceConfigurationUpdate validates a ServiceConfiguration on
// update. Runs the same consistency checks as create, plus phase
// transition and Published-phase immutability of core identity fields.
func ValidateServiceConfigurationUpdate(
	ctx context.Context,
	c client.Client,
	oldSC, newSC *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	mrtNames := collectMonitoredResourceTypeNames(newSC)
	allErrs = append(allErrs, validateMonitoredResourceTypeUniqueness(newSC)...)
	allErrs = append(allErrs, validateMeterUniqueness(newSC)...)
	allErrs = append(allErrs, validateMeterMonitoredResourceTypeRefs(newSC, mrtNames)...)
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

func collectMonitoredResourceTypeNames(sc *servicesv1alpha1.ServiceConfiguration) map[string]struct{} {
	out := make(map[string]struct{}, len(sc.Spec.MonitoredResourceTypes))
	for _, mrt := range sc.Spec.MonitoredResourceTypes {
		if mrt.Type != "" {
			out[mrt.Type] = struct{}{}
		}
	}
	return out
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

func validateMeterUniqueness(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "meters")

	seen := make(map[string]int, len(sc.Spec.Meters))
	for i, m := range sc.Spec.Meters {
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

func validateMeterMonitoredResourceTypeRefs(
	sc *servicesv1alpha1.ServiceConfiguration,
	mrtNames map[string]struct{},
) field.ErrorList {
	var allErrs field.ErrorList
	metersPath := field.NewPath("spec", "meters")

	for i, m := range sc.Spec.Meters {
		refPath := metersPath.Index(i).Child("monitoredResourceTypes")
		if len(m.MonitoredResourceTypes) == 0 {
			allErrs = append(allErrs, field.Required(
				refPath, "meter must reference at least one monitored resource type",
			))
			continue
		}
		for j, ref := range m.MonitoredResourceTypes {
			if _, ok := mrtNames[ref]; !ok {
				allErrs = append(allErrs, field.Invalid(
					refPath.Index(j), ref,
					"must match a spec.monitoredResourceTypes[].type in this ServiceConfiguration",
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
	c client.Client,
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

	metersPath := field.NewPath("spec", "meters")
	for i, m := range sc.Spec.Meters {
		if m.Name == "" {
			continue
		}
		if !strings.HasPrefix(m.Name, prefix) || strings.TrimPrefix(m.Name, prefix) == "" {
			allErrs = append(allErrs, field.Invalid(
				metersPath.Index(i).Child("name"), m.Name,
				fmt.Sprintf("must be prefixed with the referenced service %q (e.g. %q)",
					prefix, prefix+"example-meter"),
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

	oldMetersByName := make(map[string]servicesv1alpha1.MeterSpec, len(oldSC.Spec.Meters))
	for _, m := range oldSC.Spec.Meters {
		oldMetersByName[m.Name] = m
	}
	newMetersByName := make(map[string]struct{}, len(newSC.Spec.Meters))
	for _, m := range newSC.Spec.Meters {
		newMetersByName[m.Name] = struct{}{}
	}
	metersPath := field.NewPath("spec", "meters")
	for oldName := range oldMetersByName {
		if _, ok := newMetersByName[oldName]; !ok {
			allErrs = append(allErrs, field.Forbidden(
				metersPath,
				fmt.Sprintf("meter %q cannot be removed or renamed once the ServiceConfiguration is Published", oldName),
			))
		}
	}
	for i, newMeter := range newSC.Spec.Meters {
		oldMeter, ok := oldMetersByName[newMeter.Name]
		if !ok {
			continue
		}
		itemPath := metersPath.Index(i)
		if oldMeter.Measurement.Aggregation != newMeter.Measurement.Aggregation {
			allErrs = append(allErrs, field.Forbidden(
				itemPath.Child("measurement", "aggregation"),
				"measurement.aggregation is immutable once the ServiceConfiguration is Published",
			))
		}
		if oldMeter.Measurement.Unit != newMeter.Measurement.Unit {
			allErrs = append(allErrs, field.Forbidden(
				itemPath.Child("measurement", "unit"),
				"measurement.unit is immutable once the ServiceConfiguration is Published",
			))
		}
	}

	return allErrs
}
