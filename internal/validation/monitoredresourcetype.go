// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// dnsLabelRegex matches a DNS-1123 label. Used to constrain
// MonitoredResourceType label names so that the vocabulary stays
// compatible with systems that key usage events by label name.
var dnsLabelRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// MonitoredResourceTypeValidationOptions carries the context needed
// for cross-resource validation of a MonitoredResourceType.
type MonitoredResourceTypeValidationOptions struct {
	Context context.Context
	Client  client.Client
}

// ValidateMonitoredResourceTypeCreate validates a
// MonitoredResourceType on creation.
func ValidateMonitoredResourceTypeCreate(
	mrt *servicesv1alpha1.MonitoredResourceType,
	opts MonitoredResourceTypeValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateResourceTypeNameConsistency(mrt)...)
	allErrs = append(allErrs, validateOwnerMatchesGroup(mrt)...)
	allErrs = append(allErrs, validateMonitoredResourceLabels(mrt)...)
	allErrs = append(allErrs, validateMRTOwnerServiceExists(mrt, opts)...)

	return allErrs
}

// ValidateMonitoredResourceTypeUpdate validates a
// MonitoredResourceType on update. CRD-level CEL enforces immutability
// of the core fields; this provides defense-in-depth and re-runs the
// cross-resource and label checks.
func ValidateMonitoredResourceTypeUpdate(
	oldMRT, newMRT *servicesv1alpha1.MonitoredResourceType,
	opts MonitoredResourceTypeValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList

	if oldMRT.Spec.ResourceTypeName != newMRT.Spec.ResourceTypeName {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "resourceTypeName"),
			"resourceTypeName is immutable",
		))
	}
	if oldMRT.Spec.Owner.Service != newMRT.Spec.Owner.Service {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "owner", "service"),
			"owner.service is immutable",
		))
	}
	if oldMRT.Spec.GVK.Group != newMRT.Spec.GVK.Group {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "gvk", "group"),
			"gvk.group is immutable",
		))
	}
	if oldMRT.Spec.GVK.Kind != newMRT.Spec.GVK.Kind {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "gvk", "kind"),
			"gvk.kind is immutable",
		))
	}

	allErrs = append(allErrs, validateResourceTypeNameConsistency(newMRT)...)
	allErrs = append(allErrs, validateOwnerMatchesGroup(newMRT)...)
	allErrs = append(allErrs, validateMonitoredResourceLabels(newMRT)...)
	allErrs = append(allErrs, ValidatePhaseTransition(
		oldMRT.Spec.Phase, newMRT.Spec.Phase,
		field.NewPath("spec", "phase"),
	)...)

	return allErrs
}

// validateResourceTypeNameConsistency enforces that spec.resourceTypeName
// equals "<gvk.group>/<gvk.kind>". A mismatch means the canonical name
// would silently drift away from the Kind it binds.
func validateResourceTypeNameConsistency(mrt *servicesv1alpha1.MonitoredResourceType) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "resourceTypeName")

	if mrt.Spec.GVK.Group == "" || mrt.Spec.GVK.Kind == "" {
		// CRD requires both; skip to avoid a confusing double error.
		return allErrs
	}

	expected := mrt.Spec.GVK.Group + "/" + mrt.Spec.GVK.Kind
	if mrt.Spec.ResourceTypeName != expected {
		allErrs = append(allErrs, field.Invalid(
			fldPath, mrt.Spec.ResourceTypeName,
			fmt.Sprintf("must equal \"<spec.gvk.group>/<spec.gvk.kind>\" (expected %q)", expected),
		))
	}
	return allErrs
}

// validateOwnerMatchesGroup enforces that the owning service owns the
// API group of the Kind it binds. The service registry treats a
// reverse-DNS serviceName as authoritative over its API group, so a
// mismatch would break the ownership model.
func validateOwnerMatchesGroup(mrt *servicesv1alpha1.MonitoredResourceType) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "owner", "service")

	if mrt.Spec.Owner.Service == "" || mrt.Spec.GVK.Group == "" {
		return allErrs
	}
	if mrt.Spec.Owner.Service != mrt.Spec.GVK.Group {
		allErrs = append(allErrs, field.Invalid(
			fldPath, mrt.Spec.Owner.Service,
			fmt.Sprintf("must equal spec.gvk.group %q; the owning service must own the API group of the Kind",
				mrt.Spec.GVK.Group),
		))
	}
	return allErrs
}

// validateMonitoredResourceLabels enforces unique, DNS-label-shaped
// label names on the declared label vocabulary.
func validateMonitoredResourceLabels(mrt *servicesv1alpha1.MonitoredResourceType) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "labels")

	seen := make(map[string]int, len(mrt.Spec.Labels))
	for i, lbl := range mrt.Spec.Labels {
		itemPath := fldPath.Index(i).Child("name")
		if lbl.Name == "" {
			allErrs = append(allErrs, field.Required(itemPath, "label name is required"))
			continue
		}
		if !dnsLabelRegex.MatchString(lbl.Name) {
			allErrs = append(allErrs, field.Invalid(
				itemPath, lbl.Name,
				"must be a DNS-1123 label (lowercase alphanumerics and hyphens, must start and end with an alphanumeric)",
			))
			continue
		}
		if prev, ok := seen[lbl.Name]; ok {
			allErrs = append(allErrs, field.Duplicate(itemPath, lbl.Name))
			_ = prev
			continue
		}
		seen[lbl.Name] = i
	}
	return allErrs
}

// validateMRTOwnerServiceExists confirms a Service with a matching
// spec.serviceName exists in the cluster.
func validateMRTOwnerServiceExists(
	mrt *servicesv1alpha1.MonitoredResourceType,
	opts MonitoredResourceTypeValidationOptions,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "owner", "service")

	if opts.Client == nil || mrt.Spec.Owner.Service == "" {
		return allErrs
	}

	var services servicesv1alpha1.ServiceList
	if err := opts.Client.List(opts.Context, &services,
		client.MatchingFields{serviceServiceNameFieldKey: mrt.Spec.Owner.Service},
	); err != nil {
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to list services: %w", err)))
		return allErrs
	}
	if len(services.Items) == 0 {
		allErrs = append(allErrs, field.Invalid(
			fldPath, mrt.Spec.Owner.Service,
			fmt.Sprintf("no Service with spec.serviceName %q exists", mrt.Spec.Owner.Service),
		))
	}
	return allErrs
}
