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

// ApprovePermission is the IAM permission that grants the ability to set or
// change the provider's approval decision (spec.approval) on a
// ServiceConsumer. Callers without the manage permission may only mutate
// spec.approval if they hold this permission. Whether a caller holds it is
// determined by a SubjectAccessReview in the webhook layer, not by inspecting
// the username.
const ApprovePermission = "services.miloapis.com/serviceconsumers.approve"

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
			"service consumer records are created automatically by the platform; you need the \""+ManagePermission+"\" permission to create one directly",
		))
	}
	return allErrs
}

// ValidateServiceConsumerUpdate enforces the provider-only write surface:
//
//   - spec.serviceRef and spec.consumerProjectRef are the record's identity and
//     are immutable after creation for everyone, including manage-holders; only
//     spec.approval (and status) may change post-create.
//   - Callers without the manage permission may only mutate spec.approval, and
//     may do so only if they hold the approve permission.
//   - Once approval is Denied the decision cannot be changed.
//
// Callers with the manage permission bypass the approval-only restriction so
// the controller can keep spec in sync as the model evolves, but they too are
// bound by the identity-field immutability and the Denied-is-final rule.
func ValidateServiceConsumerUpdate(
	canManage, canApprove bool,
	oldSC, newSC *servicesv1alpha1.ServiceConsumer,
) field.ErrorList {
	var allErrs field.ErrorList

	// The record's identity fields are immutable after creation for everyone.
	if oldSC.Spec.ServiceRef != newSC.Spec.ServiceRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceRef"),
			"spec.serviceRef identifies this consumer record and can't be changed after creation",
		))
	}
	if oldSC.Spec.ConsumerProjectRef != newSC.Spec.ConsumerProjectRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "consumerProjectRef"),
			"spec.consumerProjectRef identifies this consumer record and can't be changed after creation",
		))
	}

	approvalChanged := !reflect.DeepEqual(oldSC.Spec.Approval, newSC.Spec.Approval)

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
				"without the \""+ManagePermission+"\" permission you can only change the approval decision, not other fields",
			))
		}

		// Changing the approval decision requires the approve permission.
		if approvalChanged && !canApprove {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "approval"),
				"you need the \""+ApprovePermission+"\" permission to set or change the approval decision",
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
				"once a request has been denied the decision can't be changed; the consumer must remove and recreate the request to try again",
			))
		}
	}

	return allErrs
}
