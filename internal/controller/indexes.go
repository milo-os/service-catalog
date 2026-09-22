// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// entitlementServiceNameIndex is the field-index key for looking up
// ServiceEntitlements by the canonical service name the entitlement
// reconciler stamps on status.serviceName. The stamped name is the single
// source of truth for matching: unstamped entitlements are not indexed and
// therefore don't match.
const entitlementServiceNameIndex = "status.serviceName"

// entitlementServiceNameIndexer extracts the value for
// entitlementServiceNameIndex.
func entitlementServiceNameIndexer(obj client.Object) []string {
	ent := obj.(*servicesv1alpha1.ServiceEntitlement)
	if ent.Status.ServiceName == "" {
		return nil
	}
	return []string{ent.Status.ServiceName}
}

// entitlementRequesterNameIndex is the field-index key for looking up
// ServiceEntitlements by spec.requestedBy.name. Registered by
// ContactEnrollmentReconciler so a Contact arriving (or changing) can be
// mapped back to the entitlements it might unblock, without a full list scan
// of every engaged project.
const entitlementRequesterNameIndex = "spec.requestedBy.name"

// entitlementRequesterNameIndexer extracts the value for
// entitlementRequesterNameIndex.
func entitlementRequesterNameIndexer(obj client.Object) []string {
	ent := obj.(*servicesv1alpha1.ServiceEntitlement)
	if ent.Spec.RequestedBy == nil || ent.Spec.RequestedBy.Name == "" {
		return nil
	}
	return []string{ent.Spec.RequestedBy.Name}
}
