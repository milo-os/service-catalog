// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// Field index keys are duplicated as string literals so the validation
// package does not depend on the controller package. They must match
// the constants exported from internal/controller/indexers.go.
const (
	meterDefinitionMeterNameFieldKey = ".spec.meterName"
	serviceServiceNameFieldKey       = ".spec.serviceName"
)

// ucumRegex is a loose UCUM shape check: a non-empty string of
// printable ASCII that excludes whitespace. Full UCUM parsing is
// deferred; this catches the common mistakes (leading/trailing
// whitespace, control characters, empty annotations like "{}").
var ucumRegex = regexp.MustCompile(`^[\x21-\x7E]{1,64}$`)

// MeterDefinitionValidationOptions carries the context needed for
// cross-resource validation of a MeterDefinition.
type MeterDefinitionValidationOptions struct {
	Context context.Context
	Client  client.Client
}

// ValidateMeterDefinitionCreate validates a MeterDefinition on
// creation, including cross-resource checks against Service and
// sibling MeterDefinition objects.
func ValidateMeterDefinitionCreate(
	md *servicesv1alpha1.MeterDefinition,
	opts MeterDefinitionValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateMeterNamePrefix(md)...)
	allErrs = append(allErrs, validateMeterUnits(md)...)
	allErrs = append(allErrs, validateMeterNameUnique(md, opts)...)
	allErrs = append(allErrs, validateMeterOwnerServiceExists(md, opts)...)

	return allErrs
}

// ValidateMeterDefinitionUpdate validates a MeterDefinition on update.
// CRD-level CEL enforces immutability of the core fields; this provides
// defense-in-depth and runs the same cross-resource checks as create.
func ValidateMeterDefinitionUpdate(
	oldMD, newMD *servicesv1alpha1.MeterDefinition,
	opts MeterDefinitionValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList

	if oldMD.Spec.MeterName != newMD.Spec.MeterName {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "meterName"),
			"meterName is immutable",
		))
	}
	if oldMD.Spec.Owner.Service != newMD.Spec.Owner.Service {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "owner", "service"),
			"owner.service is immutable",
		))
	}
	if oldMD.Spec.Measurement.Aggregation != newMD.Spec.Measurement.Aggregation {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "measurement", "aggregation"),
			"measurement.aggregation is immutable",
		))
	}
	if oldMD.Spec.Measurement.Unit != newMD.Spec.Measurement.Unit {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "measurement", "unit"),
			"measurement.unit is immutable",
		))
	}

	allErrs = append(allErrs, validateMeterNamePrefix(newMD)...)
	allErrs = append(allErrs, validateMeterUnits(newMD)...)
	allErrs = append(allErrs, validateDimensionsAdditive(oldMD, newMD)...)
	allErrs = append(allErrs, ValidatePhaseTransition(
		oldMD.Spec.Phase, newMD.Spec.Phase,
		field.NewPath("spec", "phase"),
	)...)

	return allErrs
}

// validateMeterNamePrefix enforces that spec.meterName is prefixed by
// the owning service and carries at least one path segment after the
// prefix.
func validateMeterNamePrefix(md *servicesv1alpha1.MeterDefinition) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "meterName")

	name := md.Spec.MeterName
	owner := md.Spec.Owner.Service
	if owner == "" {
		return allErrs
	}

	prefix := owner + "/"
	if !strings.HasPrefix(name, prefix) {
		allErrs = append(allErrs, field.Invalid(
			fldPath, name,
			fmt.Sprintf("must be prefixed with the owning service %q (e.g. %q)",
				prefix, prefix+"example-meter"),
		))
		return allErrs
	}

	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" {
		allErrs = append(allErrs, field.Invalid(
			fldPath, name,
			"must include a path segment after the owning service prefix",
		))
	}
	return allErrs
}

// validateMeterUnits runs a loose UCUM shape check on the three unit
// fields. Full UCUM parsing is deferred; we only want to reject the
// obvious mistakes (empty annotations, whitespace) at admission.
func validateMeterUnits(md *servicesv1alpha1.MeterDefinition) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateUCUMUnit(
		md.Spec.Measurement.Unit,
		field.NewPath("spec", "measurement", "unit"),
	)...)
	allErrs = append(allErrs, validateUCUMUnit(
		md.Spec.Billing.ConsumedUnit,
		field.NewPath("spec", "billing", "consumedUnit"),
	)...)
	allErrs = append(allErrs, validateUCUMUnit(
		md.Spec.Billing.PricingUnit,
		field.NewPath("spec", "billing", "pricingUnit"),
	)...)

	return allErrs
}

func validateUCUMUnit(value string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value == "" {
		allErrs = append(allErrs, field.Required(fldPath, "unit is required"))
		return allErrs
	}
	if !ucumRegex.MatchString(value) {
		allErrs = append(allErrs, field.Invalid(
			fldPath, value,
			"must be a non-empty printable ASCII UCUM string with no whitespace",
		))
	}
	return allErrs
}

// validateMeterNameUnique checks that no other MeterDefinition has the
// same spec.meterName. Uses the field index when registered; the fake
// client falls back correctly for unit tests that seed the index.
func validateMeterNameUnique(
	md *servicesv1alpha1.MeterDefinition,
	opts MeterDefinitionValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "meterName")

	if opts.Client == nil {
		return allErrs
	}

	var list servicesv1alpha1.MeterDefinitionList
	if err := opts.Client.List(opts.Context, &list,
		client.MatchingFields{meterDefinitionMeterNameFieldKey: md.Spec.MeterName},
	); err != nil {
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to list existing meter definitions: %w", err)))
		return allErrs
	}

	for i := range list.Items {
		existing := &list.Items[i]
		if existing.UID == md.UID {
			continue
		}
		allErrs = append(allErrs, field.Duplicate(fldPath, md.Spec.MeterName))
		break
	}
	return allErrs
}

// validateMeterOwnerServiceExists confirms a Service with a matching
// spec.serviceName exists in the cluster.
func validateMeterOwnerServiceExists(
	md *servicesv1alpha1.MeterDefinition,
	opts MeterDefinitionValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "owner", "service")

	if opts.Client == nil || md.Spec.Owner.Service == "" {
		return allErrs
	}

	var services servicesv1alpha1.ServiceList
	if err := opts.Client.List(opts.Context, &services,
		client.MatchingFields{serviceServiceNameFieldKey: md.Spec.Owner.Service},
	); err != nil {
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to list services: %w", err)))
		return allErrs
	}
	if len(services.Items) == 0 {
		allErrs = append(allErrs, field.Invalid(
			fldPath, md.Spec.Owner.Service,
			fmt.Sprintf("no Service with spec.serviceName %q exists", md.Spec.Owner.Service),
		))
	}
	return allErrs
}

// validateDimensionsAdditive enforces that dimensions can be added but
// not removed or reordered.
func validateDimensionsAdditive(oldMD, newMD *servicesv1alpha1.MeterDefinition) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "measurement", "dimensions")

	oldDims := oldMD.Spec.Measurement.Dimensions
	newDims := newMD.Spec.Measurement.Dimensions

	if len(newDims) < len(oldDims) {
		allErrs = append(allErrs, field.Forbidden(
			fldPath,
			"dimensions may only be added; removal is a breaking change and requires a new meter",
		))
		return allErrs
	}

	for i, old := range oldDims {
		if newDims[i] != old {
			allErrs = append(allErrs, field.Forbidden(
				fldPath.Index(i),
				fmt.Sprintf("existing dimension %q cannot be changed or reordered", old),
			))
		}
	}
	return allErrs
}
