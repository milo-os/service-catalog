// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"slices"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// These predicates keep watch events that change nothing a reconciler reads
// from re-running it. Several controllers write status on the same objects,
// and without them every write by one re-ran all the others, each of which
// then re-did its full pass to conclude nothing had changed. Create, delete
// and generic events always pass.

// metadataChanged reports a change to the object metadata any reconciler here
// may act on. It deliberately ignores resourceVersion and managedFields, which
// move on every write.
func metadataChanged(oldObj, newObj client.Object) bool {
	return oldObj.GetGeneration() != newObj.GetGeneration() ||
		oldObj.GetDeletionTimestamp().IsZero() != newObj.GetDeletionTimestamp().IsZero() ||
		!slices.Equal(oldObj.GetFinalizers(), newObj.GetFinalizers()) ||
		!apiequality.Semantic.DeepEqual(oldObj.GetLabels(), newObj.GetLabels()) ||
		!apiequality.Semantic.DeepEqual(oldObj.GetAnnotations(), newObj.GetAnnotations())
}

// serviceEntitlementPredicate drops the entitlement updates the entitlement
// reconciler cannot act on: status written by provisioning and by setup
// tracking, neither of which it reads. Everything else passes, including the
// Ready status the ServiceConsumer reconciler writes when a provider decides,
// which is how a decision reaches this reconciler.
func serviceEntitlementPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldEntitlement, okOld := e.ObjectOld.(*servicesv1alpha1.ServiceEntitlement)
			newEntitlement, okNew := e.ObjectNew.(*servicesv1alpha1.ServiceEntitlement)
			if !okOld || !okNew {
				return true
			}
			if metadataChanged(oldEntitlement, newEntitlement) ||
				!apiequality.Semantic.DeepEqual(oldEntitlement.Spec, newEntitlement.Spec) {
				return true
			}
			return !apiequality.Semantic.DeepEqual(
				statusWithout(&oldEntitlement.Status, provisioningStatusOwner, setupStatusOwner),
				statusWithout(&newEntitlement.Status, provisioningStatusOwner, setupStatusOwner))
		},
	}
}

// statusWithout returns a copy of status with the named owners' fields and
// conditions cleared.
func statusWithout(
	status *servicesv1alpha1.ServiceEntitlementStatus,
	owners ...entitlementStatusOwner,
) *servicesv1alpha1.ServiceEntitlementStatus {
	out := status.DeepCopy()
	for _, owner := range owners {
		for _, field := range owner.fields {
			switch field {
			case "provisionedResources":
				out.ProvisionedResources = nil
			case "lastProvisioningEvaluation":
				out.LastProvisioningEvaluation = nil
			case "setup":
				out.Setup = nil
			}
		}
		for _, condition := range owner.conditions {
			apimeta.RemoveStatusCondition(&out.Conditions, condition)
		}
	}
	return out
}

// provisioningEntitlementPredicate passes the entitlement changes that decide
// what provisioning does: its spec and metadata, and whether it is Active.
// Provisioning's own ledger writes and every other controller's conditions do
// not, so a settled entitlement is not re-provisioned each time one of them
// writes.
func provisioningEntitlementPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldEntitlement, okOld := e.ObjectOld.(*servicesv1alpha1.ServiceEntitlement)
			newEntitlement, okNew := e.ObjectNew.(*servicesv1alpha1.ServiceEntitlement)
			if !okOld || !okNew {
				return true
			}
			return metadataChanged(oldEntitlement, newEntitlement) ||
				oldEntitlement.Status.Phase != newEntitlement.Status.Phase
		},
	}
}

// serviceConfigurationPredicate passes spec changes and the canonical service
// name the controller stamps on status. The rest of status reports review and
// fan-out progress, which no entitlement depends on.
func serviceConfigurationPredicate() predicate.TypedPredicate[*servicesv1alpha1.ServiceConfiguration] {
	return predicate.TypedFuncs[*servicesv1alpha1.ServiceConfiguration]{
		UpdateFunc: func(e event.TypedUpdateEvent[*servicesv1alpha1.ServiceConfiguration]) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			return metadataChanged(e.ObjectOld, e.ObjectNew) ||
				e.ObjectOld.Status.ServiceName != e.ObjectNew.Status.ServiceName
		},
	}
}

// billingEntitlementPredicate passes spec changes and a change in whether the
// BillingEntitlement is Ready, the only status quota grants read.
func billingEntitlementPredicate() predicate.TypedPredicate[*billingv1alpha1.BillingEntitlement] {
	return predicate.TypedFuncs[*billingv1alpha1.BillingEntitlement]{
		UpdateFunc: func(e event.TypedUpdateEvent[*billingv1alpha1.BillingEntitlement]) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			return metadataChanged(e.ObjectOld, e.ObjectNew) ||
				apimeta.IsStatusConditionTrue(e.ObjectOld.Status.Conditions, ConditionTypeReady) !=
					apimeta.IsStatusConditionTrue(e.ObjectNew.Status.Conditions, ConditionTypeReady)
		},
	}
}

// billingAccountBindingPredicate passes spec changes and a change of phase,
// the only status quota grants read.
func billingAccountBindingPredicate() predicate.TypedPredicate[*billingv1alpha1.BillingAccountBinding] {
	return predicate.TypedFuncs[*billingv1alpha1.BillingAccountBinding]{
		UpdateFunc: func(e event.TypedUpdateEvent[*billingv1alpha1.BillingAccountBinding]) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			return metadataChanged(e.ObjectOld, e.ObjectNew) ||
				e.ObjectOld.Status.Phase != e.ObjectNew.Status.Phase
		},
	}
}

// offerPredicate passes spec changes. Quota grants read only an Offer's
// pricing, never its status.
func offerPredicate() predicate.TypedPredicate[*billingv1alpha1.Offer] {
	return predicate.TypedFuncs[*billingv1alpha1.Offer]{
		UpdateFunc: func(e event.TypedUpdateEvent[*billingv1alpha1.Offer]) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			return metadataChanged(e.ObjectOld, e.ObjectNew)
		},
	}
}
