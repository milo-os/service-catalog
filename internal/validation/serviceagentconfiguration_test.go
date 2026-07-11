// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// newAgentConfig builds a minimal ServiceAgentConfiguration for the
// streaming agent used across these tests.
func newAgentConfig(phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgentConfiguration {
	return &servicesv1alpha1.ServiceAgentConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "streaming-agent-v1"},
		Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
			ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: "streaming-agent"},
			Phase:           phase,
			Version:         "v1",
			Tools: &servicesv1alpha1.AgentTools{
				MCPServers: []servicesv1alpha1.AgentMCPServer{{
					Name:     "streaming",
					Endpoint: "http://127.0.0.1:7810/mcp",
					ToolSelector: &servicesv1alpha1.AgentToolSelector{
						Include: []string{"streams_list", "streams_get"},
					},
				}},
			},
			Metrics: []servicesv1alpha1.MetricSpec{{
				Name: "streaming.miloapis.com/agent/tool-invocations",
				Kind: servicesv1alpha1.MetricKindDelta,
				Unit: "{invocation}",
			}},
		},
	}
}

func TestValidateServiceAgentConfigurationCreate_RefChain(t *testing.T) {
	svc := publishedService("streaming") // serviceName streaming.miloapis.com
	agent := newServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhasePublished)
	reader := newFakeReader(t, svc, agent)

	valid := newAgentConfig(servicesv1alpha1.PhaseDraft)
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), reader, valid, false); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}

	dangling := newAgentConfig(servicesv1alpha1.PhaseDraft)
	dangling.Spec.ServiceAgentRef.Name = "missing-agent"
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), reader, dangling, false); len(errs) == 0 {
		t.Fatal("expected error for dangling serviceAgentRef, got none")
	}

	// Dry-run skips the reference resolution.
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), reader, dangling, true); len(errs) != 0 {
		t.Fatalf("expected dry-run to skip resolution, got %v", errs)
	}
}

func TestValidateServiceAgentConfigurationCreate_MetricPrefix(t *testing.T) {
	svc := publishedService("streaming")
	agent := newServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhasePublished)
	reader := newFakeReader(t, svc, agent)

	sac := newAgentConfig(servicesv1alpha1.PhaseDraft)
	sac.Spec.Metrics[0].Name = "other.example.com/agent/tool-invocations"
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), reader, sac, false); len(errs) == 0 {
		t.Fatal("expected metric prefix error, got none")
	}
}

func TestValidateServiceAgentConfigurationCreate_DuplicateMetric(t *testing.T) {
	sac := newAgentConfig(servicesv1alpha1.PhaseDraft)
	sac.Spec.Metrics = append(sac.Spec.Metrics, sac.Spec.Metrics[0])
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), nil, sac, true); len(errs) == 0 {
		t.Fatal("expected duplicate metric error, got none")
	}
}

func TestValidateServiceAgentConfigurationCreate_MCPServers(t *testing.T) {
	// Duplicate server names are rejected.
	dup := newAgentConfig(servicesv1alpha1.PhaseDraft)
	dup.Spec.Tools.MCPServers = append(dup.Spec.Tools.MCPServers, dup.Spec.Tools.MCPServers[0])
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), nil, dup, true); len(errs) == 0 {
		t.Fatal("expected duplicate mcp server name error, got none")
	}

	// A mutating tool that is not allow-listed is rejected.
	mut := newAgentConfig(servicesv1alpha1.PhaseDraft)
	mut.Spec.Tools.MCPServers[0].Mutating = []string{"streams_delete"}
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), nil, mut, true); len(errs) == 0 {
		t.Fatal("expected mutating-not-included error, got none")
	}

	// A mutating tool that is allow-listed passes.
	ok := newAgentConfig(servicesv1alpha1.PhaseDraft)
	ok.Spec.Tools.MCPServers[0].Mutating = []string{"streams_get"}
	if errs := ValidateServiceAgentConfigurationCreate(context.Background(), nil, ok, true); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateServiceAgentConfigurationUpdate_PhaseAndImmutability(t *testing.T) {
	// Phase regression Published -> Draft is rejected.
	oldSAC := newAgentConfig(servicesv1alpha1.PhasePublished)
	regressed := newAgentConfig(servicesv1alpha1.PhaseDraft)
	if errs := ValidateServiceAgentConfigurationUpdate(context.Background(), nil, oldSAC, regressed, true); len(errs) == 0 {
		t.Fatal("expected phase regression error, got none")
	}

	// Forward transition Published -> Deprecated is allowed.
	deprecated := newAgentConfig(servicesv1alpha1.PhaseDeprecated)
	if errs := ValidateServiceAgentConfigurationUpdate(context.Background(), nil, oldSAC, deprecated, true); len(errs) != 0 {
		t.Fatalf("expected no errors for forward transition, got %v", errs)
	}

	// serviceAgentRef is immutable.
	rehomed := newAgentConfig(servicesv1alpha1.PhasePublished)
	rehomed.Spec.ServiceAgentRef.Name = "other-agent"
	if errs := ValidateServiceAgentConfigurationUpdate(context.Background(), nil, oldSAC, rehomed, true); len(errs) == 0 {
		t.Fatal("expected serviceAgentRef immutability error, got none")
	}

	// Metric identity is locked once Published: unit change rejected.
	unitChanged := newAgentConfig(servicesv1alpha1.PhasePublished)
	unitChanged.Spec.Metrics[0].Unit = "{call}"
	if errs := ValidateServiceAgentConfigurationUpdate(context.Background(), nil, oldSAC, unitChanged, true); len(errs) == 0 {
		t.Fatal("expected metric unit immutability error, got none")
	}

	// Metric removal rejected once Published.
	removed := newAgentConfig(servicesv1alpha1.PhasePublished)
	removed.Spec.Metrics = nil
	if errs := ValidateServiceAgentConfigurationUpdate(context.Background(), nil, oldSAC, removed, true); len(errs) == 0 {
		t.Fatal("expected metric removal error, got none")
	}

	// Editing knowledge while Published is fine (latest-Published
	// selection keeps consumers on the newest document).
	knowledgeEdit := newAgentConfig(servicesv1alpha1.PhasePublished)
	knowledgeEdit.Spec.Knowledge = &servicesv1alpha1.AgentKnowledge{
		Sources: []servicesv1alpha1.AgentKnowledgeSource{{
			Type: servicesv1alpha1.AgentKnowledgeSourceMarkdown,
			URL:  "http://127.0.0.1:7810/notes.md",
		}},
	}
	if errs := ValidateServiceAgentConfigurationUpdate(context.Background(), nil, oldSAC, knowledgeEdit, true); len(errs) != 0 {
		t.Fatalf("expected knowledge edits to be allowed while Published, got %v", errs)
	}
}
