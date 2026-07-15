// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// streamcoBinding builds an AgentBinding equivalent to what the e2e leg proved
// the AgentBindingReconciler projects for the StreamCo demo (see
// hack/agent-framework-e2e/run-e2e.sh assertions). The endpoint is the raw
// provider endpoint the adapter is expected to rewrite onto the gateway.
func streamcoBinding() *servicesv1alpha1.AgentBinding {
	maxDur := int64(120)
	return &servicesv1alpha1.AgentBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "streamco-streaming-agent"},
		Spec: servicesv1alpha1.AgentBindingSpec{
			ServiceRef:           servicesv1alpha1.ServiceRef{Name: "streaming-streamco-example"},
			ServiceName:          "streaming.streamco.example",
			ServiceAgentRef:      servicesv1alpha1.ServiceAgentReference{Name: "streamco-streaming-agent"},
			ConfigurationVersion: "1.0.0",
			Knowledge: &servicesv1alpha1.AgentKnowledge{
				Sources: []servicesv1alpha1.AgentKnowledgeSource{
					{Type: servicesv1alpha1.AgentKnowledgeSourceLLMDocs, Title: "overview", URL: "http://streamco.svc/llms-full.txt"},
				},
				Concepts: []servicesv1alpha1.AgentConcept{
					{GVK: servicesv1alpha1.GVKRef{Group: "streaming.streamco.example", Kind: "Stream"}, Summary: "A live stream"},
				},
			},
			Tools: &servicesv1alpha1.AgentTools{
				MCPServers: []servicesv1alpha1.AgentMCPServer{
					{
						Name:         "streamco",
						Endpoint:     "http://streamco.agent-framework-playground.svc:7810/mcp",
						ToolSelector: &servicesv1alpha1.AgentToolSelector{Include: []string{"streams_list", "pipeline_diagnose"}},
						Mutating:     []string{},
					},
				},
			},
			Authority: &servicesv1alpha1.AgentAuthority{
				Reads:                  []servicesv1alpha1.AgentReadAuthority{{GVK: servicesv1alpha1.GVKRef{Group: "streaming.streamco.example", Kind: "*"}}},
				MaxTaskDurationSeconds: maxDur,
			},
		},
	}
}

func TestTranslateBinding_ProjectsAllFields(t *testing.T) {
	rw := rewriteConfig{GatewayBaseURL: "http://ai-gateway.svc:80/mcp", AppendServerName: true}
	doc := translateBinding(streamcoBinding(), "demo-project", rw)

	if doc.APIVersion != "services.miloapis.com/v1alpha1" {
		t.Errorf("apiVersion = %q", doc.APIVersion)
	}
	if doc.Kind != "AgentBinding" {
		t.Errorf("kind = %q", doc.Kind)
	}
	if doc.Metadata == nil || doc.Metadata.Namespace != "demo-project" || doc.Metadata.Name != "streamco-streaming-agent" {
		t.Errorf("metadata = %+v", doc.Metadata)
	}
	if doc.Spec.ServiceName != "streaming.streamco.example" {
		t.Errorf("serviceName = %q", doc.Spec.ServiceName)
	}
	if doc.Spec.ServiceRef.Name != "streaming-streamco-example" {
		t.Errorf("serviceRef = %q", doc.Spec.ServiceRef.Name)
	}
	if doc.Spec.ServiceAgentRef.Name != "streamco-streaming-agent" {
		t.Errorf("serviceAgentRef = %q", doc.Spec.ServiceAgentRef.Name)
	}
	if doc.Spec.ConfigurationVersion != "1.0.0" {
		t.Errorf("configurationVersion = %q", doc.Spec.ConfigurationVersion)
	}
	if doc.Spec.Knowledge == nil || len(doc.Spec.Knowledge.Sources) != 1 || len(doc.Spec.Knowledge.Concepts) != 1 {
		t.Fatalf("knowledge = %+v", doc.Spec.Knowledge)
	}
	if doc.Spec.Knowledge.Concepts[0].GVK.Kind != "Stream" {
		t.Errorf("concept gvk = %+v", doc.Spec.Knowledge.Concepts[0].GVK)
	}
	if doc.Spec.Authority == nil || doc.Spec.Authority.MaxTaskDurationSeconds == nil || *doc.Spec.Authority.MaxTaskDurationSeconds != 120 {
		t.Errorf("authority = %+v", doc.Spec.Authority)
	}
	if len(doc.Spec.Authority.Reads) != 1 || doc.Spec.Authority.Reads[0].GVK.Kind != "*" {
		t.Errorf("authority reads = %+v", doc.Spec.Authority.Reads)
	}
}

func TestTranslateBinding_EndpointRewrite(t *testing.T) {
	cases := []struct {
		name string
		rw   rewriteConfig
		want string
	}{
		{"append server name", rewriteConfig{GatewayBaseURL: "http://gw/mcp", AppendServerName: true}, "http://gw/mcp/streamco"},
		{"trailing slash trimmed", rewriteConfig{GatewayBaseURL: "http://gw/mcp/", AppendServerName: true}, "http://gw/mcp/streamco"},
		{"single endpoint", rewriteConfig{GatewayBaseURL: "http://gw/mcp/streamco", AppendServerName: false}, "http://gw/mcp/streamco"},
		{"pass through when empty", rewriteConfig{}, "http://streamco.agent-framework-playground.svc:7810/mcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := translateBinding(streamcoBinding(), "demo-project", tc.rw)
			got := doc.Spec.Tools.MCPServers[0].Endpoint
			if got != tc.want {
				t.Errorf("endpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

// The Envoy AI Gateway convention: a single federated /mcp endpoint (no
// per-server suffix) and tools namespaced by a backend prefix. The capability
// document must carry the federated tool names the assistant will ask for.
func TestTranslateBinding_EnvoyGatewayConvention(t *testing.T) {
	b := streamcoBinding()
	b.Spec.Tools.MCPServers[0].Mutating = []string{"streams_get"}
	rw := rewriteConfig{
		GatewayBaseURL:   "http://envoy-abc123.envoy-gateway-system.svc.cluster.local:80/mcp",
		AppendServerName: false,
		ToolPrefix:       "streamco-backend__",
	}
	doc := translateBinding(b, "demo-project", rw)
	srv := doc.Spec.Tools.MCPServers[0]

	if srv.Endpoint != "http://envoy-abc123.envoy-gateway-system.svc.cluster.local:80/mcp" {
		t.Errorf("endpoint = %q (want single /mcp, no suffix)", srv.Endpoint)
	}
	want := []string{"streamco-backend__streams_list", "streamco-backend__pipeline_diagnose"}
	if len(srv.ToolSelector.Include) != len(want) {
		t.Fatalf("include = %v", srv.ToolSelector.Include)
	}
	for i, w := range want {
		if srv.ToolSelector.Include[i] != w {
			t.Errorf("include[%d] = %q, want %q", i, srv.ToolSelector.Include[i], w)
		}
	}
	if len(srv.Mutating) != 1 || srv.Mutating[0] != "streamco-backend__streams_get" {
		t.Errorf("mutating = %v (want prefixed)", srv.Mutating)
	}
}

// With no prefix configured, tool names pass through unchanged (direct path).
func TestTranslateBinding_NoPrefixPassThrough(t *testing.T) {
	doc := translateBinding(streamcoBinding(), "demo-project", rewriteConfig{})
	got := doc.Spec.Tools.MCPServers[0].ToolSelector.Include
	if len(got) != 2 || got[0] != "streams_list" || got[1] != "pipeline_diagnose" {
		t.Errorf("include = %v, want raw names", got)
	}
}

// The assistant's MCPServer.toolSelector has no omitempty, so it must always
// serialize as an object even when the provider declared no selector.
func TestTranslateTools_ToolSelectorAlwaysPresent(t *testing.T) {
	b := streamcoBinding()
	b.Spec.Tools.MCPServers[0].ToolSelector = nil

	doc := translateBinding(b, "demo-project", rewriteConfig{})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	srv := generic["spec"].(map[string]any)["tools"].(map[string]any)["mcpServers"].([]any)[0].(map[string]any)
	if _, ok := srv["toolSelector"]; !ok {
		t.Errorf("toolSelector key absent from serialized mcpServer: %s", raw)
	}
}

// The emitted JSON must satisfy the assistant's required-field contract
// (mirrors internal/capability CapabilityDocument.Validate) — required fields
// present, knowledge source types among the known set.
func TestTranslateBinding_SatisfiesAssistantContract(t *testing.T) {
	doc := translateBinding(streamcoBinding(), "demo-project", rewriteConfig{GatewayBaseURL: "http://gw/mcp", AppendServerName: true})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	// Re-parse through a schema-shaped struct with the assistant's required
	// fields and assert the same invariants Validate() checks.
	var parsed struct {
		Spec struct {
			ServiceName          string                `json:"serviceName"`
			ServiceRef           struct{ Name string } `json:"serviceRef"`
			ServiceAgentRef      struct{ Name string } `json:"serviceAgentRef"`
			ConfigurationVersion string                `json:"configurationVersion"`
			Tools                *struct {
				MCPServers []struct {
					Name     string `json:"name"`
					Endpoint string `json:"endpoint"`
				} `json:"mcpServers"`
			} `json:"tools"`
			Knowledge *struct {
				Sources []struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"sources"`
			} `json:"knowledge"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Spec.ServiceName == "" || parsed.Spec.ServiceRef.Name == "" ||
		parsed.Spec.ServiceAgentRef.Name == "" || parsed.Spec.ConfigurationVersion == "" {
		t.Fatalf("required spec field empty: %+v", parsed.Spec)
	}
	for i, s := range parsed.Spec.Tools.MCPServers {
		if s.Name == "" || s.Endpoint == "" {
			t.Errorf("mcpServers[%d] missing name/endpoint", i)
		}
	}
	known := map[string]bool{"LLMDocs": true, "Runbook": true, "Markdown": true}
	for i, s := range parsed.Spec.Knowledge.Sources {
		if !known[s.Type] {
			t.Errorf("knowledge.sources[%d].type %q not in assistant enum", i, s.Type)
		}
		if s.URL == "" {
			t.Errorf("knowledge.sources[%d].url empty", i)
		}
	}
}

// --- handler behavior: P3 live fetch and P5 isolation ---

type fakeLister struct {
	bindings []servicesv1alpha1.AgentBinding
	err      error
}

func (f *fakeLister) ListBindings(context.Context) ([]servicesv1alpha1.AgentBinding, error) {
	return f.bindings, f.err
}

func decodeDocs(t *testing.T, body []byte) []capabilityDocument {
	t.Helper()
	var docs []capabilityDocument
	if err := json.Unmarshal(body, &docs); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	return docs
}

func TestHandler_EntitledProjectReturnsDocuments(t *testing.T) {
	h := newHandler(&fakeLister{bindings: []servicesv1alpha1.AgentBinding{*streamcoBinding()}},
		"demo-project", rewriteConfig{GatewayBaseURL: "http://gw/mcp", AppendServerName: true}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/demo-project/capability-documents", nil)
	h.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	docs := decodeDocs(t, rec.Body.Bytes())
	if len(docs) != 1 {
		t.Fatalf("want 1 document, got %d", len(docs))
	}
	if docs[0].Spec.Tools.MCPServers[0].Endpoint != "http://gw/mcp/streamco" {
		t.Errorf("endpoint not rewritten: %q", docs[0].Spec.Tools.MCPServers[0].Endpoint)
	}
}

// P5: a project with no entitlement (any name other than the backing consumer
// project) gets an empty, non-null array — no capabilities leak across the
// isolation boundary.
func TestHandler_UnentitledProjectIsolated(t *testing.T) {
	h := newHandler(&fakeLister{bindings: []servicesv1alpha1.AgentBinding{*streamcoBinding()}},
		"demo-project", rewriteConfig{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/other-project/capability-documents", nil)
	h.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got == "null\n" || got == "null" {
		t.Fatalf("body is null, want empty array: %q", got)
	}
	docs := decodeDocs(t, rec.Body.Bytes())
	if len(docs) != 0 {
		t.Fatalf("want 0 documents for unentitled project, got %d", len(docs))
	}
}

func TestHandler_ListErrorIsBadGateway(t *testing.T) {
	h := newHandler(&fakeLister{err: context.DeadlineExceeded}, "demo-project", rewriteConfig{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/demo-project/capability-documents", nil)
	h.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
