// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// serviceSlugRegex constrains metadata.name for Service resources to a
// Kubernetes DNS-1123 label style: lowercase alphanumerics and hyphens,
// must start and end with an alphanumeric, up to 63 chars.
var serviceSlugRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// reverseDNSRegex matches a reverse-DNS identifier such as
// "compute.miloapis.com": two or more lowercase DNS labels joined by
// dots. Individual labels are up to 63 chars and the whole string up to
// 253 (enforced separately via the CRD MaxLength).
var reverseDNSRegex = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`,
)

// ValidateServiceCreate validates a Service on creation.
func ValidateServiceCreate(svc *servicesv1alpha1.Service) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateServiceMetadataName(svc)...)
	allErrs = append(allErrs, validateServiceName(svc.Spec.ServiceName)...)

	return allErrs
}

// ValidateServiceUpdate validates a Service on update. CRD-level CEL
// already enforces serviceName immutability; the belt-and-suspenders
// check keeps the invariant in sync with validation tests.
func ValidateServiceUpdate(oldSvc, newSvc *servicesv1alpha1.Service) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateServiceMetadataName(newSvc)...)
	allErrs = append(allErrs, validateServiceName(newSvc.Spec.ServiceName)...)

	if oldSvc.Spec.ServiceName != newSvc.Spec.ServiceName {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceName"),
			"serviceName is immutable",
		))
	}

	allErrs = append(allErrs, ValidatePhaseTransition(
		oldSvc.Spec.Phase, newSvc.Spec.Phase,
		field.NewPath("spec", "phase"),
	)...)

	return allErrs
}

func validateServiceMetadataName(svc *servicesv1alpha1.Service) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("metadata", "name")

	name := svc.GetName()
	if name == "" {
		// Let the apiserver handle required-name errors; nothing to do here.
		return allErrs
	}
	if len(name) > 63 {
		allErrs = append(allErrs, field.Invalid(
			fldPath, name,
			"must be 63 characters or fewer",
		))
		return allErrs
	}
	if !serviceSlugRegex.MatchString(name) {
		allErrs = append(allErrs, field.Invalid(
			fldPath, name,
			"must be a DNS-1123 label (lowercase alphanumerics and hyphens, must start and end with an alphanumeric)",
		))
	}
	return allErrs
}

// validateServiceName enforces reverse-DNS shape for spec.serviceName.
// The CRD already enforces length bounds; this covers the format.
func validateServiceName(serviceName string) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "serviceName")

	if serviceName == "" {
		// Required field — CRD will reject; skip to avoid double error.
		return allErrs
	}
	if !reverseDNSRegex.MatchString(serviceName) {
		allErrs = append(allErrs, field.Invalid(
			fldPath, serviceName,
			fmt.Sprintf("must be a reverse-DNS name such as %q", "compute.miloapis.com"),
		))
	}
	return allErrs
}
