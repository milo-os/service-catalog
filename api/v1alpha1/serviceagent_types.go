// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceAgentSpec defines the desired state of a ServiceAgent.
//
// A ServiceAgent is the identity record for an AI agent capability a
// provider offers alongside a Service. Like Service, it is identity
// only: what the agent knows and which tools it may call live on
// versioned ServiceAgentConfiguration documents, never here.
//
// spec.phase is the provider-declared lifecycle intent: Draft ->
// Published -> Deprecated -> Retired. The controller reflects that
// declaration via conditions on status; it does not transition the
// phase itself.
type ServiceAgentSpec struct {
	// ServiceRef points at the Service this agent belongs to, by
	// metadata.name of the cluster-scoped Service resource. Immutable:
	// an agent cannot be re-homed to a different service; ship a new
	// ServiceAgent instead.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceRef is immutable"
	ServiceRef ServiceRef `json:"serviceRef"`

	// Phase is the provider-declared lifecycle state of this agent.
	// Allowed transitions are forward-only:
	// Draft -> Published -> Deprecated -> Retired.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// DisplayName is a human-readable name surfaced in the portal and
	// assistant UI alongside the owning service. Editable over the
	// agent's lifetime.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description is a plain-English explanation of what the agent can
	// help with. Editable over the agent's lifetime.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`
}

// ServiceAgentReference is a reference to a ServiceAgent by its
// metadata.name, mirroring ServiceReference.
type ServiceAgentReference struct {
	// Name is the metadata.name of the cluster-scoped ServiceAgent
	// resource.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ServiceAgentStatus defines the observed state of a ServiceAgent.
type ServiceAgentStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields
	// (publishedAt, conditions, observedGeneration). Phase lives on
	// spec; status mirrors it via the Published condition.
	CatalogStatus `json:",inline"`
}

// ServiceAgent is the Schema for the serviceagents API. It is the
// platform-owned identity record for an AI agent capability offered by
// a Service on Milo. Versioned agent behavior (knowledge, tools,
// authority, metrics) is declared on ServiceAgentConfiguration
// documents that reference this agent; consumer projects receive the
// projection via AgentBinding.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
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
