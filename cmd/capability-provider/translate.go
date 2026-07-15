// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// rewriteConfig controls how provider MCP endpoints are rewritten onto the
// gateway during translation. The provider declares a raw endpoint on its
// ServiceAgentConfiguration (e.g. the in-cluster StreamCo Service, or a
// loopback dev URL); consumers must never reach it directly. Every tool call
// is routed through the Envoy AI Gateway MCPRoute so it is metered and
// project-attributed, so the adapter overwrites the endpoint here.
type rewriteConfig struct {
	// GatewayBaseURL is the MCPRoute base the assistant should dial (e.g.
	// "http://ai-gateway.envoy-gateway-system.svc:80/mcp"). Empty disables
	// rewriting and endpoints pass through verbatim — useful for a
	// gateway-less smoke test.
	GatewayBaseURL string

	// AppendServerName controls the per-server path convention. When true
	// (the default), the rewritten endpoint is "<base>/<server.Name>", so a
	// single MCPRoute can fan out to several named servers by path. When
	// false, every server maps to GatewayBaseURL verbatim (single-endpoint
	// gateway).
	AppendServerName bool
}

// rewriteEndpoint returns the gateway URL a given provider MCP server should
// be reached through. A blank base is a pass-through.
func (c rewriteConfig) rewriteEndpoint(serverName string) string {
	if c.GatewayBaseURL == "" {
		return ""
	}
	base := strings.TrimRight(c.GatewayBaseURL, "/")
	if c.AppendServerName {
		return base + "/" + serverName
	}
	return base
}

// translateBinding projects one cluster-scoped AgentBinding into the
// assistant's capability-document shape, scoped to projectName and with MCP
// endpoints rewritten onto the gateway. The output is exactly what
// capability.ParseDocuments expects.
func translateBinding(
	binding *servicesv1alpha1.AgentBinding,
	projectName string,
	rw rewriteConfig,
) capabilityDocument {
	spec := binding.Spec

	doc := capabilityDocument{
		APIVersion: servicesv1alpha1.GroupVersion.String(),
		Kind:       "AgentBinding",
		Metadata: &documentMetadata{
			Name:      binding.Name,
			Namespace: projectName,
		},
		Spec: capabilitySpec{
			ServiceRef:           ref{Name: spec.ServiceRef.Name},
			ServiceName:          spec.ServiceName,
			ServiceAgentRef:      ref{Name: spec.ServiceAgentRef.Name},
			ConfigurationVersion: spec.ConfigurationVersion,
			Knowledge:            translateKnowledge(spec.Knowledge),
			Tools:                translateTools(spec.Tools, rw),
			Authority:            translateAuthority(spec.Authority),
		},
	}
	return doc
}

func translateKnowledge(k *servicesv1alpha1.AgentKnowledge) *knowledge {
	if k == nil {
		return nil
	}
	out := &knowledge{}
	for _, s := range k.Sources {
		out.Sources = append(out.Sources, knowledgeSource{
			Type:  string(s.Type),
			Title: s.Title,
			URL:   s.URL,
		})
	}
	for _, c := range k.Concepts {
		out.Concepts = append(out.Concepts, knowledgeConcept{
			GVK:     gvkRef{Group: c.GVK.Group, Kind: c.GVK.Kind},
			Summary: c.Summary,
		})
	}
	return out
}

func translateTools(t *servicesv1alpha1.AgentTools, rw rewriteConfig) *tools {
	if t == nil {
		return nil
	}
	out := &tools{}
	for _, srv := range t.MCPServers {
		endpoint := srv.Endpoint
		if rewritten := rw.rewriteEndpoint(srv.Name); rewritten != "" {
			endpoint = rewritten
		}

		var include []string
		if srv.ToolSelector != nil {
			include = append(include, srv.ToolSelector.Include...)
		}

		out.MCPServers = append(out.MCPServers, mcpServer{
			Name:         srv.Name,
			Endpoint:     endpoint,
			ToolSelector: toolSelector{Include: include},
			Mutating:     append([]string(nil), srv.Mutating...),
		})
	}
	return out
}

func translateAuthority(a *servicesv1alpha1.AgentAuthority) *authority {
	if a == nil {
		return nil
	}
	out := &authority{}
	for _, r := range a.Reads {
		out.Reads = append(out.Reads, authorityRead{
			GVK: gvkRef{Group: r.GVK.Group, Kind: r.GVK.Kind},
		})
	}
	if a.MaxTaskDurationSeconds > 0 {
		d := int(a.MaxTaskDurationSeconds)
		out.MaxTaskDurationSeconds = &d
	}
	return out
}
