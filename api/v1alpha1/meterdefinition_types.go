// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MeterAggregation describes how usage samples for a meter roll up over
// a billing period.
// +kubebuilder:validation:Enum=Sum;Max;Min;Count;UniqueCount;Latest;Average
type MeterAggregation string

const (
	MeterAggregationSum         MeterAggregation = "Sum"
	MeterAggregationMax         MeterAggregation = "Max"
	MeterAggregationMin         MeterAggregation = "Min"
	MeterAggregationCount       MeterAggregation = "Count"
	MeterAggregationUniqueCount MeterAggregation = "UniqueCount"
	MeterAggregationLatest      MeterAggregation = "Latest"
	MeterAggregationAverage     MeterAggregation = "Average"
)

// MeterDefinitionSpec defines the desired state of a MeterDefinition.
//
// The spec is organized into three cohesive blocks: identity (who/what
// this is), measurement (how the signal is captured and aggregated),
// and billing (how it crosses into commerce). Core fields (meterName,
// owner.service, measurement.aggregation, measurement.unit) are
// immutable once created; a breaking change ships as a new
// MeterDefinition with a versioned meterName.
//
// spec.phase is the provider-declared lifecycle intent: Draft ->
// Published -> Deprecated -> Retired. The controller mirrors that
// intent via conditions; it does not transition the phase itself.
//
// spec.meterName uniqueness is advisory at admission: the webhook
// rejects a create that collides with an existing meter, but two
// concurrent creates can both pass the check. The controller reconciles
// any duplicate it sees by setting Ready=False with reason
// DuplicateMeterName on every offender so the race is detectable
// downstream. To close the race entirely, set metadata.name to a
// deterministic encoding of spec.meterName so the apiserver's own
// name-uniqueness guarantee covers it.
type MeterDefinitionSpec struct {
	// MeterName is the canonical, user-facing identifier for this
	// meter. It is the cross-system join key used by invoices, rate
	// cards, marketplace listings, and FOCUS exports. Typically a
	// reverse-DNS path (e.g.
	// "compute.miloapis.com/instance/cpu-seconds"). Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="meterName is immutable"
	MeterName string `json:"meterName"`

	// Phase is the provider-declared lifecycle state of this meter.
	// Allowed transitions are forward-only:
	// Draft -> Published -> Deprecated -> Retired.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// DisplayName is a human-readable name surfaced in portals and on
	// invoices alongside the canonical meterName.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description is a plain-English explanation of what the meter
	// measures. Editable over the meter's lifetime.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Owner identifies the service that publishes and owns the meter.
	//
	// +kubebuilder:validation:Required
	Owner MeterOwner `json:"owner"`

	// Measurement describes how the signal is captured and aggregated.
	//
	// +kubebuilder:validation:Required
	Measurement MeterMeasurement `json:"measurement"`

	// Billing describes how the meter crosses into commerce. Carries no
	// rates, currencies, or tiers -- those live in the pricing engine.
	//
	// +kubebuilder:validation:Required
	Billing MeterBilling `json:"billing"`
}

// MeterOwner identifies the service that publishes and owns a meter.
type MeterOwner struct {
	// Service is the reverse-DNS name of the owning Service (e.g.
	// "compute.miloapis.com"), matching that Service's
	// spec.serviceName. The webhook enforces that the referenced
	// Service exists and that spec.meterName is prefixed with this
	// value. Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="owner.service is immutable"
	Service string `json:"service"`
}

// MeterMeasurement describes how a meter's signal is captured.
type MeterMeasurement struct {
	// Aggregation is the function applied to samples over a billing
	// period. Single source of truth for how usage rolls up. Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="measurement.aggregation is immutable"
	Aggregation MeterAggregation `json:"aggregation"`

	// Unit is a UCUM (https://ucum.org/ucum) string describing what the
	// meter measures (e.g. "s", "By", "{request}", "GBy.h"). Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="measurement.unit is immutable"
	Unit string `json:"unit"`

	// Dimensions is an ordered list of attribute keys that downstream
	// systems may group by (e.g. "region", "tier", "resource.type").
	// Adding a dimension is additive; removing one is a breaking change
	// and must ship as a new meter.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Dimensions []string `json:"dimensions,omitempty"`
}

// MeterBilling describes the commercial framing of a meter. Field names
// borrow from the FOCUS specification for clean exports.
type MeterBilling struct {
	// ConsumedUnit is the UCUM unit in which usage is measured (e.g.
	// "s"). Typically matches measurement.unit; may diverge when the
	// emitted telemetry is pre-converted (e.g. measured in "s" but
	// emitted pre-rolled in "min"). Equality with measurement.unit is
	// not enforced.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	ConsumedUnit string `json:"consumedUnit"`

	// PricingUnit is the UCUM unit pricing quotes against (e.g. "h").
	// May differ from ConsumedUnit; the pricing engine handles the
	// conversion.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	PricingUnit string `json:"pricingUnit"`
}

// MeterDefinitionStatus defines the observed state of a MeterDefinition.
type MeterDefinitionStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields
	// (publishedAt, conditions, observedGeneration). Phase lives on
	// spec; status mirrors it via the Published condition.
	CatalogStatus `json:",inline"`
}

// MeterDefinition is the Schema for the meterdefinitions API. It is a
// declarative, platform-advertised catalog entry for a single billable
// dimension -- what is measured, in what unit, how it is aggregated,
// and who owns it. It does not ingest events, calculate money, or store
// customer data.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Meter",type=string,JSONPath=`.spec.meterName`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner.service`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Aggregation",type=string,JSONPath=`.spec.measurement.aggregation`,priority=1
// +kubebuilder:printcolumn:name="Unit",type=string,JSONPath=`.spec.measurement.unit`,priority=1
type MeterDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MeterDefinitionSpec   `json:"spec,omitempty"`
	Status MeterDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MeterDefinitionList contains a list of MeterDefinition.
type MeterDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MeterDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MeterDefinition{}, &MeterDefinitionList{})
}
