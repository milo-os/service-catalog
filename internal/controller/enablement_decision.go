// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"fmt"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Ready reasons written on a ServiceConsumer. The provider reads these, so
// they describe the request from the provider's side.
const (
	reasonConsumerEnabled         = "ConsumerEnabled"
	reasonConsumerPendingApproval = "ConsumerPendingApproval"
	reasonConsumerApproved        = "ConsumerApproved"
	reasonConsumerDenied          = "ConsumerDenied"
)

// enablementDecision is how access to a service was decided for one consumer
// project, as reported on both sides: the ServiceEntitlement the consumer reads
// and the ServiceConsumer the provider reads.
//
// The entitlement and consumer reconcilers both write the entitlement's Ready
// condition. They derive it from this one function so they always write the
// same thing; when they didn't, the approval reason the consumer reconciler
// wrote was overwritten by the entitlement reconciler on its next pass.
type enablementDecision struct {
	entitlementPhase   servicesv1alpha1.EntitlementPhase
	entitlementReason  string
	entitlementMessage string

	consumerPhase   servicesv1alpha1.ConsumerPhase
	consumerReason  string
	consumerMessage string
}

// decideEnablement returns the decision for a service with the given
// enablement mode and the provider's approval, if any. A self-service
// service ignores spec.approval: there is nothing for the provider to decide.
func decideEnablement(gated bool, approval *servicesv1alpha1.ProviderApproval) enablementDecision {
	switch {
	case !gated:
		return enablementDecision{
			entitlementPhase:   servicesv1alpha1.EntitlementPhaseActive,
			entitlementReason:  servicesv1alpha1.ReasonEntitlementActive,
			entitlementMessage: "This service was enabled automatically and is ready to use.",
			consumerPhase:      servicesv1alpha1.ConsumerPhaseActive,
			consumerReason:     reasonConsumerEnabled,
			consumerMessage:    "Enabled automatically.",
		}
	case approval == nil:
		return enablementDecision{
			entitlementPhase:   servicesv1alpha1.EntitlementPhasePendingApproval,
			entitlementReason:  servicesv1alpha1.ReasonEntitlementPendingApproval,
			entitlementMessage: "Waiting for the service provider to approve this request.",
			consumerPhase:      servicesv1alpha1.ConsumerPhasePendingApproval,
			consumerReason:     reasonConsumerPendingApproval,
			consumerMessage:    "Waiting for an approval decision.",
		}
	case approval.Decision == servicesv1alpha1.ApprovalDecisionDenied:
		return enablementDecision{
			entitlementPhase:   servicesv1alpha1.EntitlementPhaseRejected,
			entitlementReason:  servicesv1alpha1.ReasonEntitlementRejected,
			entitlementMessage: withProviderNote("The service provider denied this request.", approval.Message),
			consumerPhase:      servicesv1alpha1.ConsumerPhaseDenied,
			consumerReason:     reasonConsumerDenied,
			consumerMessage:    "This request was denied; the consumer project does not have access to the service.",
		}
	default:
		return enablementDecision{
			entitlementPhase:   servicesv1alpha1.EntitlementPhaseActive,
			entitlementReason:  servicesv1alpha1.ReasonEntitlementApproved,
			entitlementMessage: withProviderNote("The service provider approved this request. This service is enabled and ready to use.", approval.Message),
			consumerPhase:      servicesv1alpha1.ConsumerPhaseActive,
			consumerReason:     reasonConsumerApproved,
			consumerMessage:    "This request was approved; the consumer project has access to the service.",
		}
	}
}

// withProviderNote appends the provider's decision message, when there is one,
// so the consumer sees why the provider decided as it did.
func withProviderNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return fmt.Sprintf("%s Note from the provider: %s", msg, note)
}
