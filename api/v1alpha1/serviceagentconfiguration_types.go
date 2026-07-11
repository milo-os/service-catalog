// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceAgentConfigurationSpec defines the desired state of a
// ServiceAgentConfiguration.
//
// A ServiceAgentConfiguration is the single provider-facing document
// that describes everything a ServiceAgent contributes beyond its
// identity record: the knowledge sources the assistant may ground on,
// the MCP tools it may call, the authority envelope it operates under,
// and the usage metrics it emits. Documents are versioned by creating
// new resources; the LATEST Published configuration for an agent is the
// active one (the same selection approach LocationBinding takes for
// ServiceConfiguration).
//
// spec.phase is the provider-declared lifecycle intent:
// Draft -> Published -> Deprecated -> Retired. Draft documents are
// never projected to consumers. The controller mirrors that intent via
// conditions; it does not transition the phase itself.
type ServiceAgentConfigurationSpec struct {
	// ServiceAgentRef points at the ServiceAgent this document
	// configures, by metadata.name of the cluster-scoped ServiceAgent
	// resource.
	//
	// +kubebuilder:validation:Required
	ServiceAgentRef ServiceAgentReference `json:"serviceAgentRef"`

	// Phase is the provider-declared lifecycle state of this
	// configuration. Allowed transitions are forward-only:
	// Draft -> Published -> Deprecated -> Retired.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// Version is an optional human-readable version string for this
	// configuration document (e.g. "v1", "2024-01-15"). It has no
	// semantic meaning to the controller and is surfaced as a table
	// column and on projected AgentBindings for operator convenience.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`

	// Knowledge declares the Tier 1 grounding material for the agent:
	// documents the assistant may fetch and concepts it should know.
	//
	// +kubebuilder:validation:Optional
	Knowledge *AgentKnowledge `json:"knowledge,omitempty"`

	// Tools declares the Tier 2 MCP tool surface for the agent.
	//
	// +kubebuilder:validation:Optional
	Tools *AgentTools `json:"tools,omitempty"`

	// Delegation declares the Tier 3 agent-to-agent delegation surface.
	//
	// +kubebuilder:validation:Optional
	Delegation *AgentDelegation `json:"delegation,omitempty"`

	// Authority declares the envelope the agent operates under on
	// behalf of a consumer: which Kinds it may read and how long a
	// single task may run.
	//
	// +kubebuilder:validation:Optional
	Authority *AgentAuthority `json:"authority,omitempty"`

	// Metrics declares metric descriptors for agent usage (e.g. tool
	// invocations). Entries mirror ServiceConfiguration.spec.metrics and
	// must be prefixed by the owning Service's canonical serviceName.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=name
	Metrics []MetricSpec `json:"metrics,omitempty"`
}

// AgentKnowledgeSourceType classifies a knowledge source document.
//
// +kubebuilder:validation:Enum=LLMDocs;Runbook;Markdown
type AgentKnowledgeSourceType string

const (
	// AgentKnowledgeSourceLLMDocs is an llms.txt / llms-full.txt style
	// service overview document.
	AgentKnowledgeSourceLLMDocs AgentKnowledgeSourceType = "LLMDocs"

	// AgentKnowledgeSourceRunbook is an operational runbook.
	AgentKnowledgeSourceRunbook AgentKnowledgeSourceType = "Runbook"

	// AgentKnowledgeSourceMarkdown is a general markdown document.
	AgentKnowledgeSourceMarkdown AgentKnowledgeSourceType = "Markdown"
)

// AgentKnowledge groups the grounding material declared by a
// ServiceAgentConfiguration.
type AgentKnowledge struct {
	// Sources lists documents the assistant may fetch and append to its
	// context, labeled with provenance.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Sources []AgentKnowledgeSource `json:"sources,omitempty"`

	// Concepts lists the Kubernetes Kinds the agent should understand,
	// each with a short summary.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Concepts []AgentConcept `json:"concepts,omitempty"`
}

// AgentKnowledgeSource is a single fetchable knowledge document.
type AgentKnowledgeSource struct {
	// Type classifies the document.
	//
	// +kubebuilder:validation:Required
	Type AgentKnowledgeSourceType `json:"type"`

	// Title is an optional human-readable label for the document.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	Title string `json:"title,omitempty"`

	// URL is where the document is served from.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
}

// AgentConcept ties a Kubernetes Kind to a short summary the agent can
// ground on.
type AgentConcept struct {
	// GVK pins the concept to a Kubernetes Kind. Version is
	// deliberately omitted, mirroring MonitoredResourceTypeSpec.
	//
	// +kubebuilder:validation:Required
	GVK GVKRef `json:"gvk"`

	// Summary is a plain-English explanation of the Kind.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Summary string `json:"summary"`
}

// AgentTools groups the MCP tool surface declared by a
// ServiceAgentConfiguration.
type AgentTools struct {
	// MCPServers lists the MCP servers the assistant may connect to on
	// the consumer's behalf. Entries are keyed by .name, which must be
	// unique within the document.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	MCPServers []AgentMCPServer `json:"mcpServers,omitempty"`
}

// AgentMCPServer declares one MCP server endpoint and the allow-listed
// tools it exposes.
type AgentMCPServer struct {
	// Name is a short DNS-label identifier for the server. Downstream
	// consumers namespace exposed tools as "<name>__<tool>".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Endpoint is the Streamable HTTP endpoint of the MCP server.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`

	// ToolSelector allow-lists the tools consumers may call. Tools not
	// in the include list are never exposed, regardless of what the
	// server advertises.
	//
	// +kubebuilder:validation:Optional
	ToolSelector *AgentToolSelector `json:"toolSelector,omitempty"`

	// Mutating lists the subset of included tools that mutate state.
	// Downstream policy may require confirmation before invoking them.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Mutating []string `json:"mutating,omitempty"`
}

// AgentToolSelector allow-lists MCP tools by name.
type AgentToolSelector struct {
	// Include lists the tool names to expose.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Include []string `json:"include,omitempty"`
}

// AgentDelegation groups agent-to-agent delegation declarations.
type AgentDelegation struct {
	// A2A declares the A2A agent card for delegation discovery.
	//
	// +kubebuilder:validation:Optional
	A2A *AgentA2ASpec `json:"a2a,omitempty"`
}

// AgentA2ASpec declares an A2A agent card and the skills it advertises.
type AgentA2ASpec struct {
	// AgentCardURL is where the agent-card.json document is served.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	AgentCardURL string `json:"agentCardUrl"`

	// Skills lists the skill identifiers the agent advertises.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Skills []string `json:"skills,omitempty"`
}

// AgentAuthority is the envelope the agent operates under on behalf of
// a consumer project.
type AgentAuthority struct {
	// Reads lists the Kubernetes Kinds the agent may read in the
	// consumer project. Kind may be "*" to allow every Kind in the
	// group.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Reads []AgentReadAuthority `json:"reads,omitempty"`

	// MaxTaskDurationSeconds bounds how long a single agent task may
	// run.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	MaxTaskDurationSeconds int64 `json:"maxTaskDurationSeconds,omitempty"`
}

// AgentReadAuthority grants read access to one Kubernetes Kind (or a
// whole group via kind "*").
type AgentReadAuthority struct {
	// GVK identifies the readable Kind. Version is deliberately
	// omitted; kind may be "*".
	//
	// +kubebuilder:validation:Required
	GVK GVKRef `json:"gvk"`
}

// ServiceAgentConfigurationStatus defines the observed state of a
// ServiceAgentConfiguration.
type ServiceAgentConfigurationStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields
	// (publishedAt, conditions, observedGeneration).
	CatalogStatus `json:",inline"`

	// ServiceName is the resolved canonical reverse-DNS name of the
	// Service backing the referenced ServiceAgent (e.g.
	// "streaming.streamco.example"). Populated by the controller after
	// the serviceAgentRef chain is successfully resolved.
	//
	// +kubebuilder:validation:Optional
	ServiceName string `json:"serviceName,omitempty"`
}

// ServiceAgentConfiguration is the Schema for the
// serviceagentconfigurations API. It is the versioned provider-facing
// document declaring an agent's knowledge, tools, authority, and usage
// metrics. metadata.name conventionally carries the agent slug plus a
// version suffix (e.g. "streamco-streaming-agent-v1") to make the
// versioning obvious at a glance.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.serviceAgentRef.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
type ServiceAgentConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceAgentConfigurationSpec   `json:"spec,omitempty"`
	Status ServiceAgentConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceAgentConfigurationList contains a list of
// ServiceAgentConfiguration.
type ServiceAgentConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceAgentConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceAgentConfiguration{}, &ServiceAgentConfigurationList{})
}
