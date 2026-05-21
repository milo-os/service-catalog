// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"reflect"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// IsServicesControllerCaller reports whether the admission user appears
// to be the services controller (or another in-cluster service account).
// Provider users come in as regular users; the controller comes in as a
// system:serviceaccount.
func IsServicesControllerCaller(user authenticationv1.UserInfo) bool {
	name := user.Username
	if strings.HasPrefix(name, "system:serviceaccount:") {
		return true
	}
	if strings.Contains(strings.ToLower(name), "services-controller") {
		return true
	}
	return false
}

// ValidateServiceConsumerCreate rejects creates from non-controller
// callers. Only the services controller should ever create a
// ServiceConsumer; providers interact via spec.approval on update.
func ValidateServiceConsumerCreate(
	user authenticationv1.UserInfo,
	sc *servicesv1alpha1.ServiceConsumer,
) field.ErrorList {
	var allErrs field.ErrorList
	if !IsServicesControllerCaller(user) {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("metadata", "name"),
			"ServiceConsumer objects may only be created by the services controller",
		))
	}
	return allErrs
}

// ValidateServiceConsumerUpdate enforces the provider-only write surface:
// provider callers may only mutate spec.approval, and once approval is
// Denied the decision cannot be changed. The controller bypasses these
// restrictions so it can keep status/spec in sync as the model evolves.
func ValidateServiceConsumerUpdate(
	user authenticationv1.UserInfo,
	oldSC, newSC *servicesv1alpha1.ServiceConsumer,
) field.ErrorList {
	var allErrs field.ErrorList

	controllerCaller := IsServicesControllerCaller(user)

	if !controllerCaller {
		// Provider callers may only touch spec.approval. Compare the rest
		// of the spec; reject if anything else changed.
		oldNoApproval := oldSC.Spec
		newNoApproval := newSC.Spec
		oldNoApproval.Approval = nil
		newNoApproval.Approval = nil
		if !reflect.DeepEqual(oldNoApproval, newNoApproval) {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec"),
				"only spec.approval may be modified by provider callers",
			))
		}
	}

	// Once Denied, the decision is immutable for everyone — the
	// consumer must delete the ServiceEntitlement and recreate to reset
	// the flow.
	if oldSC.Spec.Approval != nil &&
		oldSC.Spec.Approval.Decision == servicesv1alpha1.ApprovalDecisionDenied {
		if newSC.Spec.Approval == nil ||
			newSC.Spec.Approval.Decision != servicesv1alpha1.ApprovalDecisionDenied {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "approval", "decision"),
				"approval.decision is immutable once set to Denied",
			))
		}
	}

	return allErrs
}
