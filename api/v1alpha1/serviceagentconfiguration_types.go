// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceAgentConfigurationSpec is the reviewed content a ServiceAgent
// contributes: documents the assistant may read, tools it may call, guides it
// may follow, and the limits it works within. New versions ship as new objects;
// the latest Published one is the one customers get.
//
// The field names below are copied verbatim into the CapabilityBinding written
// into each customer's project, so the JSON tags must stay in step with the
// assistant's own schema. Renaming one here does not break any build — it just
// stops that piece of content from ever reaching a customer.
type ServiceAgentConfigurationSpec struct {
	// ServiceAgentRef is the ServiceAgent this document configures.
	//
	// +kubebuilder:validation:Required
	ServiceAgentRef ServiceAgentReference `json:"serviceAgentRef"`

	// Phase is how far along the provider says this document is. Only
	// Published documents reach customer projects.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// Version is the provider's own stamp for this content (e.g. "v1"). It is
	// copied onto every projected binding and is how we tell which content a
	// customer actually saw, so it must not be blank.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`

	// Knowledge is what the assistant may read about this service.
	//
	// +kubebuilder:validation:Optional
	Knowledge *AgentKnowledge `json:"knowledge,omitempty"`

	// Tools is what the assistant may call.
	//
	// +kubebuilder:validation:Optional
	Tools *AgentTools `json:"tools,omitempty"`

	// Skills are step-by-step guides the assistant may follow. Only the name
	// and description are shown to it; the guide itself is fetched when it
	// looks relevant, so publishing many of them costs almost nothing.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Skills []AgentSkill `json:"skills,omitempty"`

	// Authority is what the assistant may look at and for how long.
	//
	// +kubebuilder:validation:Optional
	Authority *AgentAuthority `json:"authority,omitempty"`

	// ReportingProject is the provider's own Milo project, where their team
	// reads reports of things the assistant could not do. Leave it empty and
	// those reports have nowhere to go — the assistant simply stops offering
	// to file them.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	ReportingProject string `json:"reportingProject,omitempty"`
}

// AgentKnowledgeSourceType says what kind of document a knowledge source is.
//
// +kubebuilder:validation:Enum=LLMDocs;Runbook;Markdown
type AgentKnowledgeSourceType string

const (
	// AgentKnowledgeSourceLLMDocs is an llms.txt style service overview.
	AgentKnowledgeSourceLLMDocs AgentKnowledgeSourceType = "LLMDocs"

	// AgentKnowledgeSourceRunbook is an operational runbook.
	AgentKnowledgeSourceRunbook AgentKnowledgeSourceType = "Runbook"

	// AgentKnowledgeSourceMarkdown is a general markdown document.
	AgentKnowledgeSourceMarkdown AgentKnowledgeSourceType = "Markdown"
)

// AgentKnowledge is what the assistant may read about a service.
type AgentKnowledge struct {
	// Sources are documents the assistant fetches when it needs them. An
	// unreachable one is skipped, not an error.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Sources []AgentKnowledgeSource `json:"sources,omitempty"`

	// Concepts are short explanations of this service's resources, so basic
	// questions can be answered without fetching anything.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Concepts []AgentConcept `json:"concepts,omitempty"`
}

// AgentKnowledgeSource is one fetchable document.
type AgentKnowledgeSource struct {
	// +kubebuilder:validation:Required
	Type AgentKnowledgeSourceType `json:"type"`

	// Title is how the document is credited when its text is used.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	Title string `json:"title,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
}

// AgentConcept is a short explanation of one resource kind.
type AgentConcept struct {
	// +kubebuilder:validation:Required
	GVK GVKRef `json:"gvk"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Summary string `json:"summary"`
}

// AgentTools is what the assistant may call.
type AgentTools struct {
	// MCPServers are the tool servers the assistant may connect to.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	MCPServers []AgentMCPServer `json:"mcpServers,omitempty"`
}

// AgentMCPServer is one tool server and the tools approved on it.
type AgentMCPServer struct {
	// Name prefixes this server's tools as "<name>__<tool>", so two providers
	// can offer a tool of the same name without clashing.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Endpoint is the server's address. In production this must point at the
	// AI gateway: a call sent straight to the service is not billed, does not
	// carry the customer's identity, and is refused.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`

	// ToolSelector is the approved list of tools. Anything not named there is
	// never offered to the assistant, even if the server serves it.
	//
	// +kubebuilder:validation:Optional
	ToolSelector *AgentToolSelector `json:"toolSelector,omitempty"`

	// Mutating names the approved tools that change a customer's resources,
	// so the assistant asks before using them. Leaving one out of this list
	// means it can be used without asking.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Mutating []string `json:"mutating,omitempty"`
}

// AgentToolSelector is the approved list of tool names.
type AgentToolSelector struct {
	// Include is the approved list. An empty list means no tools from this
	// server, never "all of them".
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Include []string `json:"include,omitempty"`
}

// AgentSkill is one step-by-step guide the assistant may follow. It grants
// nothing on its own — it can only point at tools that are already approved.
type AgentSkill struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Description is the one line the assistant sees, and the only thing it
	// can match a request against. A guide nobody can pick is wasted.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description"`

	// Source is where the guide itself is served from.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Source string `json:"source"`
}

// AgentAuthority is what the assistant may look at, and for how long.
type AgentAuthority struct {
	// Reads are the resource kinds the assistant may read in a customer's
	// project. Reads happen as the person asking, so this grants no access
	// they do not already have. Kind may be "*" for every kind in the group.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Reads []AgentReadAuthority `json:"reads,omitempty"`

	// MaxTaskDurationSeconds caps how long one task using this service's tools
	// may run. Leave it unset to use the assistant's own default.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	MaxTaskDurationSeconds *int64 `json:"maxTaskDurationSeconds,omitempty"`
}

// AgentReadAuthority is one readable resource kind.
type AgentReadAuthority struct {
	// +kubebuilder:validation:Required
	GVK GVKRef `json:"gvk"`
}

// ServiceAgentConfigurationStatus is the observed state of a
// ServiceAgentConfiguration.
type ServiceAgentConfigurationStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields (publishedAt,
	// conditions, observedGeneration).
	CatalogStatus `json:",inline"`

	// ServiceName is the service's full name, resolved through the referenced
	// agent. Tool usage is billed against it.
	//
	// +kubebuilder:validation:Optional
	ServiceName string `json:"serviceName,omitempty"`
}

// ServiceAgentConfiguration is one reviewed version of what a ServiceAgent
// offers. Name it after the agent plus a version suffix (for example
// "compute-diagnostics-agent-v1") so versions are obvious at a glance.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.serviceAgentRef.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +genclient
// +genclient:nonNamespaced
type ServiceAgentConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceAgentConfigurationSpec   `json:"spec,omitempty"`
	Status ServiceAgentConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceAgentConfigurationList contains a list of ServiceAgentConfiguration.
type ServiceAgentConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceAgentConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceAgentConfiguration{}, &ServiceAgentConfigurationList{})
}
