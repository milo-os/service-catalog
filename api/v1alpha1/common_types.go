// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase describes the publication lifecycle of a governance catalog entry
// (Service, MeterDefinition, MonitoredResourceType). The same four-state
// machine applies to every catalog kind so that downstream systems
// (billing, portal, marketplace) can reason about visibility and
// referenceability uniformly.
//
// Transitions are user-driven: catalog publishing is self-service, and
// the controller does not gate moves between phases. Breaking changes
// ship as a brand-new resource with a new canonical name rather than by
// mutating an existing one.
//
//	Draft      -> Published -> Deprecated -> Retired
//
// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
type Phase string

const (
	// PhaseDraft indicates the resource is being iterated on and is not
	// yet visible to downstream systems. References to a Draft resource
	// are rejected by admission controllers.
	PhaseDraft Phase = "Draft"

	// PhasePublished is the steady state. The canonical identifier and
	// any fields marked immutable are locked; cosmetic fields
	// (descriptions, display names, additive extensions) may still evolve.
	PhasePublished Phase = "Published"

	// PhaseDeprecated indicates the resource is winding down. Existing
	// references continue to work, but the resource is hidden from new
	// onboarding flows and dashboards may surface a deprecation warning.
	PhaseDeprecated Phase = "Deprecated"

	// PhaseRetired indicates the resource is no longer referenceable.
	// The record is preserved for audit and historical lookup only.
	PhaseRetired Phase = "Retired"
)

// ProducerProjectReference is a typed reference to the producer project
// that owns a service. Using a typed reference rather than a free-text
// team string is deliberate: it prevents the drift (typos, forks,
// rebrands) that makes identity unreliable across billing, portal, and
// marketplace.
type ProducerProjectReference struct {
	// Name is the metadata.name of the producer-project resource that
	// owns the service. The webhook will validate that a producer
	// project with this name exists; the API type here only enforces
	// the syntactic bounds.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// CatalogStatus is the shared observed-state shape for the three
// governance catalog resources. It is embedded as a value into each
// concrete Status so the JSON shape stays flat (no extra envelope) while
// the Go definitions remain DRY.
//
// Phase is declared intent and lives on spec; status carries only the
// controller-observed artifacts of that intent (PublishedAt, Conditions,
// ObservedGeneration).
type CatalogStatus struct {
	// PublishedAt is the time at which the controller first observed
	// the resource in the Published phase. It is preserved across
	// later transitions to Deprecated and Retired so downstream systems
	// can reason about how long the resource has been part of the
	// catalog.
	//
	// +kubebuilder:validation:Optional
	PublishedAt *metav1.Time `json:"publishedAt,omitempty"`

	// Conditions represent the latest available observations of the
	// resource's state.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
