// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentBindingSpec defines the desired state of an AgentBinding.
//
// An AgentBinding is the consumer-facing projection of a Published
// ServiceAgent and its active (latest Published)
// ServiceAgentConfiguration into an entitled consumer project. The
// spec is fully denormalized -- knowledge, tools, and authority are
// copied verbatim from the active configuration -- so consumers never
// read platform-scoped catalog objects.
//
// AgentBindings are written exclusively by the services operator when
// all three gates are open (ServiceEntitlement Active, ServiceAgent
// Published, at least one Published ServiceAgentConfiguration) and
// pruned when any gate closes. They carry a controller owner reference
// to the ServiceEntitlement so garbage collection reclaims them if the
// operator misses a delete.
type AgentBindingSpec struct {
	// ServiceRef identifies the Service the binding belongs to, by
	// metadata.name, mirroring ServiceEntitlement.spec.serviceRef.
	//
	// +kubebuilder:validation:Required
	ServiceRef ServiceRef `json:"serviceRef"`

	// ServiceName is the canonical reverse-DNS identifier of the
	// service (e.g. "streaming.streamco.example"), denormalized from
	// Service.spec.serviceName so consumers can attribute usage without
	// a platform lookup.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ServiceName string `json:"serviceName"`

	// ServiceAgentRef identifies the projected ServiceAgent by
	// metadata.name.
	//
	// +kubebuilder:validation:Required
	ServiceAgentRef ServiceAgentReference `json:"serviceAgentRef"`

	// ConfigurationVersion is the spec.version of the active
	// ServiceAgentConfiguration this binding was projected from.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	ConfigurationVersion string `json:"configurationVersion,omitempty"`

	// Knowledge is copied from the active configuration's
	// spec.knowledge.
	//
	// +kubebuilder:validation:Optional
	Knowledge *AgentKnowledge `json:"knowledge,omitempty"`

	// Tools is copied from the active configuration's spec.tools.
	//
	// +kubebuilder:validation:Optional
	Tools *AgentTools `json:"tools,omitempty"`

	// Authority is copied from the active configuration's
	// spec.authority.
	//
	// +kubebuilder:validation:Optional
	Authority *AgentAuthority `json:"authority,omitempty"`
}

// AgentBindingStatus defines the observed state of an AgentBinding.
type AgentBindingStatus struct {
	// Conditions represent the latest available observations of the
	// binding's state.
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

// AgentBinding is the Schema for the agentbindings API. It is the
// consumer-project projection of a Published ServiceAgent: everything
// the assistant runtime needs (knowledge sources, allow-listed MCP
// tools, authority envelope) denormalized into the consumer's virtual
// control plane by the services operator.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.serviceAgentRef.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.configurationVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type AgentBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentBindingSpec   `json:"spec,omitempty"`
	Status AgentBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentBindingList contains a list of AgentBinding.
type AgentBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentBinding{}, &AgentBindingList{})
}
