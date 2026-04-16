// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// phaseTransitionMessage is the error text surfaced when a phase
// transition would skip forward, go backward, or otherwise violate the
// declared lifecycle. Kept as a single constant so every catalog kind
// (Service, MeterDefinition, MonitoredResourceType) rejects with the
// same wording.
const phaseTransitionMessage = "phase may only transition forward: Draft\u2192Published\u2192Deprecated\u2192Retired"

// phaseOrder maps each phase to its position in the forward lifecycle.
// An unknown phase (including an empty string) returns -1.
func phaseOrder(p servicesv1alpha1.Phase) int {
	switch p {
	case servicesv1alpha1.PhaseDraft:
		return 0
	case servicesv1alpha1.PhasePublished:
		return 1
	case servicesv1alpha1.PhaseDeprecated:
		return 2
	case servicesv1alpha1.PhaseRetired:
		return 3
	default:
		return -1
	}
}

// ValidatePhaseTransition enforces the forward-only lifecycle:
// Draft -> Published -> Deprecated -> Retired. The no-op transition
// X -> X is always allowed; any other move is rejected. Draft may only
// step to Published (it cannot skip straight to Deprecated or Retired).
//
// The returned field.ErrorList targets the provided fldPath so callers
// can share this helper across Spec shapes.
func ValidatePhaseTransition(oldPhase, newPhase servicesv1alpha1.Phase, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// If the phase did not change, there is nothing to validate.
	if oldPhase == newPhase {
		return allErrs
	}

	// The enum is enforced by the CRD; a value outside the known set
	// still gets flagged here to keep the defense-in-depth consistent.
	oldIdx := phaseOrder(oldPhase)
	newIdx := phaseOrder(newPhase)
	if newIdx < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, string(newPhase), phaseTransitionMessage))
		return allErrs
	}
	// An empty/unknown old phase can advance to any valid phase.
	if oldIdx < 0 {
		return allErrs
	}

	// Forward single-step only: newIdx must equal oldIdx + 1.
	if newIdx != oldIdx+1 {
		allErrs = append(allErrs, field.Invalid(fldPath, string(newPhase), phaseTransitionMessage))
	}
	return allErrs
}
