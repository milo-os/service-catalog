// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newServiceAgent(name, serviceRef string, phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgent {
	return &servicesv1alpha1.ServiceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceAgentSpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: serviceRef},
			Phase:       phase,
			DisplayName: "Example Agent",
		},
	}
}

func TestValidateServiceAgentCreate_ServiceRefMustResolve(t *testing.T) {
	svc := publishedService("streaming")
	reader := newFakeReader(t, svc)

	valid := newServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhaseDraft)
	if errs := ValidateServiceAgentCreate(context.Background(), reader, valid, false); len(errs) != 0 {
		t.Fatalf("expected no errors for resolvable serviceRef, got %v", errs)
	}

	dangling := newServiceAgent("streaming-agent", "missing", servicesv1alpha1.PhaseDraft)
	if errs := ValidateServiceAgentCreate(context.Background(), reader, dangling, false); len(errs) == 0 {
		t.Fatal("expected error for dangling serviceRef, got none")
	}

	// Dry-run admissions skip the reference lookup, mirroring the
	// ServiceConfiguration webhook behavior.
	if errs := ValidateServiceAgentCreate(context.Background(), reader, dangling, true); len(errs) != 0 {
		t.Fatalf("expected dry-run to skip serviceRef resolution, got %v", errs)
	}
}

func TestValidateServiceAgentUpdate_ServiceRefImmutable(t *testing.T) {
	oldSA := newServiceAgent("streaming-agent", "streaming", servicesv1alpha1.PhaseDraft)
	newSA := newServiceAgent("streaming-agent", "other-service", servicesv1alpha1.PhaseDraft)
	if errs := ValidateServiceAgentUpdate(oldSA, newSA); len(errs) == 0 {
		t.Fatal("expected serviceRef immutability error, got none")
	}

	unchanged := ValidateServiceAgentUpdate(oldSA, oldSA.DeepCopy())
	if len(unchanged) != 0 {
		t.Fatalf("expected no errors for unchanged update, got %v", unchanged)
	}
}

func TestValidateServiceAgentUpdate_PhaseTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    servicesv1alpha1.Phase
		to      servicesv1alpha1.Phase
		wantErr bool
	}{
		{"draft to published", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhasePublished, false},
		{"published to deprecated", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseDeprecated, false},
		{"published to draft (regression)", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseDraft, true},
		{"draft to retired (skip)", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhaseRetired, true},
		{"no-op", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhasePublished, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSA := newServiceAgent("streaming-agent", "streaming", tt.from)
			newSA := newServiceAgent("streaming-agent", "streaming", tt.to)
			errs := ValidateServiceAgentUpdate(oldSA, newSA)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateServiceAgentUpdate(%s->%s) errs = %v, wantErr %v", tt.from, tt.to, errs, tt.wantErr)
			}
		})
	}
}
