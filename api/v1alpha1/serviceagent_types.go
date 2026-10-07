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

	// Visibility asks which customer projects this agent reaches. Omitted, it
	// means what it always has: only projects entitled to the service.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	Visibility ServiceAgentVisibility `json:"visibility,omitempty"`
}

const (
	// VisibilityEntitlementRequired gates on an Active ServiceEntitlement for
	// the service in the project. The default.
	VisibilityEntitlementRequired = "Required"

	// VisibilityEntitlementNone asks for every project to get the agent,
	// entitled or not. Honoured only for agents the platform owner allowlists.
	VisibilityEntitlementNone = "None"
)

// ServiceAgentVisibility gates which projects receive an agent's
// CapabilityBinding. It mirrors PluginVisibility, the setting that lets a
// portal plugin reach every project, so one word means the same thing for
// everything a service publishes to customers.
type ServiceAgentVisibility struct {
	// Entitlement controls project-level gating: "Required" means a project
	// must have an Active ServiceEntitlement for this service before its
	// assistant is given this agent; "None" asks for every project's
	// assistant to be given it, existing and new, with no entitlement step
	// (see milo-os/service-catalog#93). Defaults to "Required".
	//
	// "None" is a request, not a grant. Which services are platform-wide is
	// the platform owner's decision, so it takes effect only when this
	// agent's name is in the services operator's
	// capabilities.entitlementFreeAgents allowlist, which providers cannot
	// write. Until then the agent is gated on entitlement as if it said
	// "Required", and its EntitlementWaived condition says why.
	//
	// Phase and configuration gates still apply: the agent must be
	// Published, its Service must be Published, and it must have a Published
	// ServiceAgentConfiguration.
	//
	// Where the operator runs with consumer-scoped projection, it writes only
	// into the projects it has engaged as consumers of its own services, so
	// "None" reaches every one of those, not every project on the platform.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Required;None
	// +kubebuilder:default=Required
	Entitlement string `json:"entitlement,omitempty"`
}

// RequestsNoEntitlement reports whether the agent asks to reach every project
// rather than only entitled ones. Whether that is honoured depends on the
// operator's allowlist. An unset value is the default, Required.
func (a *ServiceAgent) RequestsNoEntitlement() bool {
	return a.Spec.Visibility.Entitlement == VisibilityEntitlementNone
}

const (
	// ConditionTypeEntitlementWaived is set on a ServiceAgent that asks for
	// spec.visibility.entitlement None. True when the operator honours it,
	// False when the agent is not allowlisted and so is still gated on
	// entitlement. Absent on agents that ask for Required.
	ConditionTypeEntitlementWaived = "EntitlementWaived"

	// ReasonAllowlisted is the EntitlementWaived=True reason.
	ReasonAllowlisted = "Allowlisted"

	// ReasonNotAllowlisted is the EntitlementWaived=False reason.
	ReasonNotAllowlisted = "NotAllowlisted"
)

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
// configuration into a CapabilityBinding in every entitled customer project,
// or in every project when spec.visibility.entitlement is None and the
// platform owner has allowlisted the agent.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Entitlement",type=string,JSONPath=`.spec.visibility.entitlement`
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
