// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceConfigurationSpec defines the desired state of a
// ServiceConfiguration.
//
// A ServiceConfiguration is the single provider-facing document that
// describes everything a service contributes to Milo beyond its identity
// record: its monitored resource types (the Kubernetes Kinds billing and
// dashboards know about) and its meters (the billable dimensions those
// Kinds emit). The services operator fans this document out into the
// downstream CRDs consumed by billing; providers never author those
// directly.
//
// Canonical names on meters and monitored resource types must still be
// prefixed by the referenced service's spec.serviceName. The webhook
// resolves spec.serviceRef and enforces the prefix; the API type only
// constrains the shape.
//
// spec.phase is the provider-declared lifecycle intent:
// Draft -> Published -> Deprecated -> Retired. Draft documents are not
// fanned out. The controller mirrors that intent via conditions; it does
// not transition the phase itself.
type ServiceConfigurationSpec struct {
	// ServiceRef points at the Service this document configures. The
	// reference is by metadata.name of the cluster-scoped Service
	// resource; the webhook resolves it to the Service's canonical
	// spec.serviceName for prefix enforcement.
	//
	// +kubebuilder:validation:Required
	ServiceRef ServiceReference `json:"serviceRef"`

	// Phase is the provider-declared lifecycle state of this
	// configuration. Allowed transitions are forward-only:
	// Draft -> Published -> Deprecated -> Retired.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// MonitoredResourceTypes declares the Kubernetes Kinds this service
	// emits usage for, together with the closed set of labels each
	// Kind's usage events may carry. Entries are keyed by .type, which
	// must be unique within the document.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=type
	MonitoredResourceTypes []MonitoredResourceTypeSpec `json:"monitoredResourceTypes,omitempty"`

	// Meters declares the billable dimensions this service emits, each
	// bound to one or more of the monitored resource types declared
	// above. Entries are keyed by .name, which must be unique within
	// the document.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=name
	Meters []MeterSpec `json:"meters,omitempty"`
}

// ServiceReference identifies the Service a ServiceConfiguration applies
// to by metadata.name. The webhook resolves the reference to the
// Service's canonical spec.serviceName for name-prefix enforcement.
type ServiceReference struct {
	// Name is the metadata.name of the cluster-scoped Service resource
	// this ServiceConfiguration configures.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// MeterSpec is a single billable dimension declared by a
// ServiceConfiguration. It carries the same measurement/billing shape
// that billing's MeterDefinition consumes; the fan-out maps it across
// verbatim.
type MeterSpec struct {
	// Name is the canonical, user-facing identifier for this meter
	// (e.g. "compute.miloapis.com/instance/cpu-seconds"). Must be
	// prefixed by the referenced Service's spec.serviceName and unique
	// within spec.meters. Immutable once the ServiceConfiguration is
	// Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// DisplayName is a human-readable name surfaced in portals and on
	// invoices alongside the canonical name.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Description is a plain-English explanation of what the meter
	// measures.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Measurement describes how the signal is captured and aggregated.
	//
	// +kubebuilder:validation:Required
	Measurement MeterMeasurement `json:"measurement"`

	// Billing describes how the meter crosses into commerce. Carries
	// no rates, currencies, or tiers -- those live in the pricing
	// engine.
	//
	// +kubebuilder:validation:Required
	Billing MeterBilling `json:"billing"`

	// MonitoredResourceTypes binds this meter to the monitored
	// resource types that emit it. Each entry must match a
	// spec.monitoredResourceTypes[].type in the same document. At
	// least one entry is required.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	MonitoredResourceTypes []string `json:"monitoredResourceTypes"`
}

// MeterMeasurement describes how a meter's signal is captured.
type MeterMeasurement struct {
	// Aggregation is the function applied to samples over a billing
	// period. Immutable once the ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	Aggregation MeterAggregation `json:"aggregation"`

	// Unit is a UCUM (https://ucum.org/ucum) string describing what
	// the meter measures (e.g. "s", "By", "{request}"). Immutable once
	// the ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Unit string `json:"unit"`
}

// MeterBilling describes the commercial framing of a meter. Field
// names borrow from the FOCUS specification for clean exports.
type MeterBilling struct {
	// ConsumedUnit is the UCUM unit in which usage is measured (e.g.
	// "s"). Typically matches measurement.unit; may diverge when the
	// emitted telemetry is pre-rolled (e.g. measured in "s" but
	// emitted pre-rolled in "min").
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

// MonitoredResourceTypeSpec is a monitored resource type declared by
// a ServiceConfiguration. The fan-out produces one
// billing.miloapis.com/MonitoredResourceType per entry.
type MonitoredResourceTypeSpec struct {
	// Type is the canonical, user-facing identifier for this resource
	// type (e.g. "compute.miloapis.com/Instance"). Must be prefixed by
	// the referenced Service's spec.serviceName and unique within
	// spec.monitoredResourceTypes. Immutable once the
	// ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Type string `json:"type"`

	// DisplayName is a human-readable name surfaced in portals and
	// dashboards alongside the canonical type.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Description is a plain-English explanation of what the resource
	// type represents.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// GVK pins the resource type to a Kubernetes Kind. Version is
	// deliberately omitted: billability is a property of the Kind, not
	// of a specific API version. Immutable once the
	// ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	GVK GVKRef `json:"gvk"`

	// Labels is the closed set of descriptive labels that usage events
	// against this resource type are permitted to carry. Events whose
	// labels are not in this set are rejected at the edge.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Labels []MonitoredResourceLabel `json:"labels,omitempty"`
}

// GVKRef identifies a Kubernetes Kind by group and kind. Version is
// intentionally excluded so API version evolution does not require a
// new monitored resource type entry.
type GVKRef struct {
	// Group is the Kubernetes API group of the Kind (e.g.
	// "compute.miloapis.com").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Group string `json:"group"`

	// Kind is the Kubernetes Kind (e.g. "Instance").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`
}

// MonitoredResourceLabel declares a single descriptive label that
// usage events against the resource type may carry.
type MonitoredResourceLabel struct {
	// Name is the label key as it will appear on usage events (e.g.
	// "region", "zone", "tier"). It is the map key for the enclosing
	// list.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Description is a plain-English explanation of what the label
	// conveys.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=512
	Description string `json:"description,omitempty"`
}

// ServiceConfigurationStatus defines the observed state of a
// ServiceConfiguration. The controller records compact top-level
// conditions here; per-item status lives on the downstream billing
// objects themselves.
type ServiceConfigurationStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields
	// (publishedAt, conditions, observedGeneration).
	CatalogStatus `json:",inline"`
}

// ServiceConfiguration is the Schema for the serviceconfigurations API.
// It is the single provider-facing document that declares everything a
// service contributes to Milo beyond its identity record. metadata.name
// is conventionally the service's reverse-DNS slug (e.g.
// "compute-miloapis-com") to make the 1:1 relationship between Service
// and ServiceConfiguration obvious at a glance.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ServiceConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceConfigurationSpec   `json:"spec,omitempty"`
	Status ServiceConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceConfigurationList contains a list of ServiceConfiguration.
type ServiceConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceConfiguration{}, &ServiceConfigurationList{})
}
