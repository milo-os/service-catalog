// SPDX-License-Identifier: AGPL-3.0-only

package main

// This file mirrors the assistant's capability-document schema
// (milo-os/assistant internal/capability/document.go). The assistant OWNS
// that schema; this adapter conforms to it, never the reverse. The JSON tags
// here are byte-for-byte what the assistant unmarshals, so a document emitted
// by this adapter parses cleanly through capability.ParseDocuments and passes
// CapabilityDocument.Validate.
//
// We deliberately do NOT marshal the service-catalog AgentBinding object
// directly: its metav1.ObjectMeta would leak resourceVersion, uid,
// managedFields, ownerReferences and a null creationTimestamp onto the wire.
// The assistant ignores unknown fields, so that would parse — but the
// published contract is a clean, minimal document, and translating field by
// field is what keeps this adapter honest about the schema it serves.

// capabilityDocument is one project-scoped entitlement document.
type capabilityDocument struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   *documentMetadata `json:"metadata,omitempty"`
	Spec       capabilitySpec    `json:"spec"`
	Status     *documentStatus   `json:"status,omitempty"`
}

// documentMetadata carries the object identity. Namespace is the consumer
// project the document is scoped to — the same slot the assistant fixtures
// use to record "demo-project".
type documentMetadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// capabilitySpec is the projected entitlement: which provider service it
// grants and the knowledge/tools/authority it carries.
type capabilitySpec struct {
	ServiceRef           ref        `json:"serviceRef"`
	ServiceName          string     `json:"serviceName"`
	ServiceAgentRef      ref        `json:"serviceAgentRef"`
	ConfigurationVersion string     `json:"configurationVersion"`
	Knowledge            *knowledge `json:"knowledge,omitempty"`
	Tools                *tools     `json:"tools,omitempty"`
	Authority            *authority `json:"authority,omitempty"`
}

type ref struct {
	Name string `json:"name"`
}

type gvkRef struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
}

type knowledge struct {
	Sources  []knowledgeSource  `json:"sources,omitempty"`
	Concepts []knowledgeConcept `json:"concepts,omitempty"`
}

type knowledgeSource struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
}

type knowledgeConcept struct {
	GVK     gvkRef `json:"gvk"`
	Summary string `json:"summary"`
}

type tools struct {
	MCPServers []mcpServer `json:"mcpServers,omitempty"`
}

// mcpServer matches the assistant's MCPServer: toolSelector is a value (not a
// pointer) with no omitempty, so it is always present on the wire.
type mcpServer struct {
	Name         string       `json:"name"`
	Endpoint     string       `json:"endpoint"`
	ToolSelector toolSelector `json:"toolSelector"`
	Mutating     []string     `json:"mutating,omitempty"`
}

type toolSelector struct {
	Include []string `json:"include,omitempty"`
}

type authority struct {
	Reads                  []authorityRead `json:"reads,omitempty"`
	MaxTaskDurationSeconds *int            `json:"maxTaskDurationSeconds,omitempty"`
}

type authorityRead struct {
	GVK gvkRef `json:"gvk"`
}

type documentStatus struct {
	Conditions []documentCondition `json:"conditions,omitempty"`
}

type documentCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}
