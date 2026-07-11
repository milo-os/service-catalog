// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ValidateServiceAgentCreate validates a ServiceAgent on creation. It
// rejects the request when spec.serviceRef.name does not resolve to an
// existing Service — mirroring how ServiceConfiguration resolves its
// serviceRef at admission.
func ValidateServiceAgentCreate(
	ctx context.Context,
	c client.Reader,
	sa *servicesv1alpha1.ServiceAgent,
	isDryRun bool,
) field.ErrorList {
	var allErrs field.ErrorList
	if !isDryRun {
		allErrs = append(allErrs, validateServiceAgentServiceRef(ctx, c, sa)...)
	}
	return allErrs
}

// ValidateServiceAgentUpdate validates a ServiceAgent on update.
// spec.serviceRef is immutable (CRD-level CEL already enforces this;
// the belt-and-suspenders check keeps the invariant in sync with
// validation tests) and spec.phase may only transition forward.
func ValidateServiceAgentUpdate(
	oldSA, newSA *servicesv1alpha1.ServiceAgent,
) field.ErrorList {
	var allErrs field.ErrorList

	if oldSA.Spec.ServiceRef.Name != newSA.Spec.ServiceRef.Name {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceRef", "name"),
			"serviceRef is immutable",
		))
	}

	allErrs = append(allErrs, ValidatePhaseTransition(
		oldSA.Spec.Phase, newSA.Spec.Phase,
		field.NewPath("spec", "phase"),
	)...)

	return allErrs
}

// validateServiceAgentServiceRef resolves spec.serviceRef.name to a
// Service resource.
func validateServiceAgentServiceRef(
	ctx context.Context,
	c client.Reader,
	sa *servicesv1alpha1.ServiceAgent,
) field.ErrorList {
	var allErrs field.ErrorList
	serviceRefPath := field.NewPath("spec", "serviceRef", "name")

	if c == nil || sa.Spec.ServiceRef.Name == "" {
		return allErrs
	}

	var svc servicesv1alpha1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: sa.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				serviceRefPath, sa.Spec.ServiceRef.Name,
				fmt.Sprintf("no Service with metadata.name %q exists", sa.Spec.ServiceRef.Name),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(serviceRefPath,
			fmt.Errorf("failed to load referenced Service: %w", err)))
	}
	return allErrs
}
