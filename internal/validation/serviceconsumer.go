// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"reflect"

	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ValidateServiceConsumerUpdate enforces the provider-only write surface.
// privilegedCaller should be true when the caller has been confirmed (e.g. via
// SubjectAccessReview for the "approve" verb) to have unrestricted write access;
// those callers bypass field-level restrictions. All callers are still subject
// to the immutability rule on Denied decisions.
func ValidateServiceConsumerUpdate(
	privilegedCaller bool,
	oldSC, newSC *servicesv1alpha1.ServiceConsumer,
) field.ErrorList {
	var allErrs field.ErrorList

	if !privilegedCaller {
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
