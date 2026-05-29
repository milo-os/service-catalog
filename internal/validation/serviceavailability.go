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

// ValidateServiceAvailabilityCreate validates a ServiceAvailability on
// creation. spec.serviceRef must resolve to a Published Service and
// spec.locationRef.name must be set.
func ValidateServiceAvailabilityCreate(
	ctx context.Context,
	c client.Reader,
	sa *servicesv1alpha1.ServiceAvailability,
) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateServiceAvailabilityServiceRef(ctx, c, sa)...)

	if sa.Spec.LocationRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			field.NewPath("spec", "locationRef", "name"),
			"a location must be specified",
		))
	}
	return allErrs
}

// ValidateServiceAvailabilityUpdate validates a ServiceAvailability on
// update. Both spec.serviceRef and spec.locationRef are immutable: the
// object records availability for one (service, location) pair for its
// entire lifetime.
func ValidateServiceAvailabilityUpdate(
	ctx context.Context,
	c client.Reader,
	oldSA, newSA *servicesv1alpha1.ServiceAvailability,
) field.ErrorList {
	var allErrs field.ErrorList

	if oldSA.Spec.ServiceRef != newSA.Spec.ServiceRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceRef"),
			"cannot change which service this availability record is for; it covers one service and location for its lifetime",
		))
	}
	if oldSA.Spec.LocationRef != newSA.Spec.LocationRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "locationRef"),
			"cannot change which location this availability record is for; it covers one service and location for its lifetime",
		))
	}
	return allErrs
}

func validateServiceAvailabilityServiceRef(
	ctx context.Context,
	c client.Reader,
	sa *servicesv1alpha1.ServiceAvailability,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "serviceRef", "name")

	if c == nil || sa.Spec.ServiceRef.Name == "" {
		return allErrs
	}

	var svc servicesv1alpha1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: sa.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				fldPath, sa.Spec.ServiceRef.Name,
				fmt.Sprintf("the service %q does not exist", sa.Spec.ServiceRef.Name),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to load referenced Service: %w", err)))
		return allErrs
	}

	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		allErrs = append(allErrs, field.Invalid(
			fldPath, sa.Spec.ServiceRef.Name,
			fmt.Sprintf("the service %q isn't published yet, so its availability can't be recorded", svc.Name),
		))
	}
	return allErrs
}
