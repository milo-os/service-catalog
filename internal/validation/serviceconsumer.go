// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"reflect"

	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ManagePermission is the IAM permission that grants full write access to a
// ServiceConsumer spec. Callers that hold it (the services controller) bypass
// the provider-only write restrictions; everyone else may only mutate
// spec.approval. Whether a caller holds it is determined by a
// SubjectAccessReview in the webhook layer, not by inspecting the username.
const ManagePermission = "services.miloapis.com/serviceconsumers.manage"

// ValidateServiceConsumerCreate rejects creates from callers without the
// manage permission. Only the services controller should create a
// ServiceConsumer; providers interact via spec.approval on update.
func ValidateServiceConsumerCreate(
	canManage bool,
	sc *servicesv1alpha1.ServiceConsumer,
) field.ErrorList {
	var allErrs field.ErrorList
	if !canManage {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("metadata", "name"),
			"ServiceConsumer objects may only be created by callers with the "+ManagePermission+" permission",
		))
	}
	return allErrs
}

// ValidateServiceConsumerUpdate enforces the provider-only write surface:
// callers without the manage permission may only mutate spec.approval, and
// once approval is Denied the decision cannot be changed. Callers with the
// manage permission bypass the spec restriction so the controller can keep
// spec in sync as the model evolves.
func ValidateServiceConsumerUpdate(
	canManage bool,
	oldSC, newSC *servicesv1alpha1.ServiceConsumer,
) field.ErrorList {
	var allErrs field.ErrorList

	if !canManage {
		// Callers without manage may only touch spec.approval. Compare the
		// rest of the spec; reject if anything else changed.
		oldNoApproval := oldSC.Spec
		newNoApproval := newSC.Spec
		oldNoApproval.Approval = nil
		newNoApproval.Approval = nil
		if !reflect.DeepEqual(oldNoApproval, newNoApproval) {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec"),
				"only spec.approval may be modified without the "+ManagePermission+" permission",
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
