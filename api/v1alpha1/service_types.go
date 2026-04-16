// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceSpec defines the desired state of a Service.
//
// A Service is the first-class identity record for a managed capability
// a provider offers on Milo. Everything consumer-facing -- the canonical
// name, the display name, the description, the owning producer project,
// and the lifecycle phase -- lives here. Runtime concerns (deployments,
// images, endpoints, routing) stay in the provider's own repository;
// this resource is identity only.
//
// The canonical identifier is spec.serviceName. Once the service is
// Published, serviceName is locked: breaking changes ship as a new
// Service with a new serviceName and a coordinated migration, never a
// silent mutation. metadata.name is the Kubernetes slug used by kubectl
// and access controls; spec.serviceName is the reverse-DNS identifier
// that appears on invoices, in the portal, and in every downstream
// reference.
//
// spec.phase is the provider-declared lifecycle intent: Draft ->
// Published -> Deprecated -> Retired. The controller reflects that
// declaration via conditions on status; it does not transition the
// phase itself.
type ServiceSpec struct {
	// ServiceName is the canonical reverse-DNS identifier for this
	// service (e.g. "compute.miloapis.com"). It is the cross-system
	// join key used by MeterDefinition, MonitoredResourceType, billing
	// exports, and the portal. Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceName is immutable"
	ServiceName string `json:"serviceName"`

	// Phase is the provider-declared lifecycle state of this Service.
	// Allowed transitions are forward-only:
	// Draft -> Published -> Deprecated -> Retired.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// DisplayName is a human-readable name surfaced in the portal,
	// marketplace, and on invoices alongside the canonical serviceName.
	// Editable over the service's lifetime.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description is a plain-English explanation of what the service
	// offers. Editable over the service's lifetime.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Owner identifies the producer project that publishes and owns
	// the service. The reference is typed rather than a free-text team
	// string so that ownership stays tied to a real, trackable resource.
	//
	// +kubebuilder:validation:Required
	Owner ServiceOwner `json:"owner"`
}

// ServiceOwner identifies who is responsible for a Service on Milo.
type ServiceOwner struct {
	// ProducerProjectRef points at the producer-project resource that
	// publishes and owns this service. The webhook resolves the
	// reference; the API type only constrains its shape.
	//
	// +kubebuilder:validation:Required
	ProducerProjectRef ProducerProjectReference `json:"producerProjectRef"`
}

// ServiceStatus defines the observed state of a Service.
type ServiceStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields
	// (publishedAt, conditions, observedGeneration). Phase lives on
	// spec; status mirrors it via the Published condition.
	CatalogStatus `json:",inline"`
}

// Service is the Schema for the services API. It is the platform-owned
// identity record for a managed service offered on Milo. Downstream
// governance resources (MeterDefinition, MonitoredResourceType) and
// future consumers (quota, marketplace, entitlements) reference a
// Service by its spec.serviceName.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceSpec   `json:"spec,omitempty"`
	Status ServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceList contains a list of Service.
type ServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Service `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Service{}, &ServiceList{})
}
