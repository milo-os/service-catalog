// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceAgentSpec is a provider's registration of the AI-assistant help it
// offers alongside a Service. It is identity only: what the assistant actually
// knows and may call lives on ServiceAgentConfiguration documents.
type ServiceAgentSpec struct {
	// ServiceRef is the Service this agent belongs to, by metadata.name.
	// Immutable — an agent cannot be moved to another service; register a new
	// one instead.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceRef is immutable"
	ServiceRef ServiceRef `json:"serviceRef"`

	// Phase is how far along the provider says this agent is. Only Published
	// agents reach customer projects; anything else is invisible to them.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// DisplayName is the name people see in the portal.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description is a plain-English note on what the agent can help with.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`
}

// ServiceAgentReference is a reference to a ServiceAgent by its metadata.name.
type ServiceAgentReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ServiceAgentStatus is the observed state of a ServiceAgent.
type ServiceAgentStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields (publishedAt,
	// conditions, observedGeneration).
	CatalogStatus `json:",inline"`
}

// ServiceAgent is a provider's agent registration for one Service. The
// projection controller turns a Published agent plus its latest Published
// configuration into a CapabilityBinding in every entitled customer project.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +genclient
// +genclient:nonNamespaced
type ServiceAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceAgentSpec   `json:"spec,omitempty"`
	Status ServiceAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceAgentList contains a list of ServiceAgent.
type ServiceAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceAgent{}, &ServiceAgentList{})
}
