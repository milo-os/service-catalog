// SPDX-License-Identifier: AGPL-3.0-only

package controller

import billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

// dimensionsShrink reports whether desired removes any dimension name present
// on the existing MeterDefinition. Billing webhooks reject subtractive dimension
// updates, so fan-out must delete and recreate instead of server-side apply.
func dimensionsShrink(existing, desired []string) bool {
	if len(existing) == 0 {
		return false
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, name := range desired {
		desiredSet[name] = struct{}{}
	}
	for _, name := range existing {
		if _, ok := desiredSet[name]; !ok {
			return true
		}
	}
	return false
}

// mrtLabelNamesShrink reports whether desired removes any label name present on
// the existing MonitoredResourceType. Billing webhooks reject subtractive label
// updates for the same reason as meter dimensions.
func mrtLabelNamesShrink(existing, desired []billingv1alpha1.MonitoredResourceLabel) bool {
	if len(existing) == 0 {
		return false
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, label := range desired {
		desiredSet[label.Name] = struct{}{}
	}
	for _, label := range existing {
		if _, ok := desiredSet[label.Name]; !ok {
			return true
		}
	}
	return false
}
