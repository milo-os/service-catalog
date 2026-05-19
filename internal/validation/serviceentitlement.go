// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// ValidateServiceEntitlementCreate validates a ServiceEntitlement on
// creation. It rejects the request when spec.serviceRef.name does not
// resolve to a Published Service.
func ValidateServiceEntitlementCreate(
	ctx context.Context,
	c client.Reader,
	se *servicesv1alpha1.ServiceEntitlement,
) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateServiceEntitlementServiceRef(ctx, c, se)...)
	return allErrs
}

// ValidateServiceEntitlementUpdate validates a ServiceEntitlement on
// update. spec.serviceRef.name is immutable; the rest of the spec is
// reconciler-managed.
func ValidateServiceEntitlementUpdate(
	ctx context.Context,
	c client.Reader,
	oldSE, newSE *servicesv1alpha1.ServiceEntitlement,
) field.ErrorList {
	var allErrs field.ErrorList

	if oldSE.Spec.ServiceRef.Name != newSE.Spec.ServiceRef.Name {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceRef", "name"),
			"serviceRef.name is immutable",
		))
	}
	return allErrs
}

// ValidateServiceEntitlementDelete refuses to delete a dependency
// entitlement while the parent entitlement that pulled it in is still
// Active. Direct entitlements are always free to delete.
func ValidateServiceEntitlementDelete(
	ctx context.Context,
	c client.Reader,
	se *servicesv1alpha1.ServiceEntitlement,
) field.ErrorList {
	var allErrs field.ErrorList

	if se.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
		return allErrs
	}
	if se.Status.DependencyOf == "" || c == nil {
		return allErrs
	}

	var parent servicesv1alpha1.ServiceEntitlement
	if err := c.Get(ctx, types.NamespacedName{Name: se.Status.DependencyOf}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			// Parent gone — dependency entitlement is free to drop.
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(field.NewPath("status", "dependencyOf"),
			fmt.Errorf("failed to load parent ServiceEntitlement: %w", err)))
		return allErrs
	}

	if parent.Status.Phase == servicesv1alpha1.EntitlementPhaseActive {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("metadata", "name"),
			fmt.Sprintf("dependency entitlement cannot be deleted while parent entitlement %q is Active", parent.Name),
		))
	}
	return allErrs
}

func validateServiceEntitlementServiceRef(
	ctx context.Context,
	c client.Reader,
	se *servicesv1alpha1.ServiceEntitlement,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "serviceRef", "name")

	if c == nil || se.Spec.ServiceRef.Name == "" {
		return allErrs
	}

	var svc servicesv1alpha1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: se.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				fldPath, se.Spec.ServiceRef.Name,
				fmt.Sprintf("no Service with metadata.name %q exists", se.Spec.ServiceRef.Name),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to load referenced Service: %w", err)))
		return allErrs
	}

	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		allErrs = append(allErrs, field.Invalid(
			fldPath, se.Spec.ServiceRef.Name,
			fmt.Sprintf("Service %q is in phase %q; only Published Services can be entitled", svc.Name, svc.Spec.Phase),
		))
	}
	return allErrs
}
