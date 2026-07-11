// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ValidateServiceAgentConfigurationCreate validates a
// ServiceAgentConfiguration on creation. Runs intra-document
// consistency checks plus the ServiceAgent -> Service chain lookup for
// metric name-prefix enforcement, mirroring
// ValidateServiceConfigurationCreate.
func ValidateServiceAgentConfigurationCreate(
	ctx context.Context,
	c client.Reader,
	sac *servicesv1alpha1.ServiceAgentConfiguration,
	isDryRun bool,
) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateAgentMetricUniqueness(sac)...)
	allErrs = append(allErrs, validateAgentMCPServers(sac)...)
	if !isDryRun {
		allErrs = append(allErrs, validateServiceAgentConfigurationRefs(ctx, c, sac)...)
	}

	return allErrs
}

// ValidateServiceAgentConfigurationUpdate validates a
// ServiceAgentConfiguration on update. Runs the same consistency checks
// as create, plus phase transition and Published-phase immutability of
// metric identity fields.
func ValidateServiceAgentConfigurationUpdate(
	ctx context.Context,
	c client.Reader,
	oldSAC, newSAC *servicesv1alpha1.ServiceAgentConfiguration,
	isDryRun bool,
) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateAgentMetricUniqueness(newSAC)...)
	allErrs = append(allErrs, validateAgentMCPServers(newSAC)...)
	if !isDryRun {
		allErrs = append(allErrs, validateServiceAgentConfigurationRefs(ctx, c, newSAC)...)
	}
	allErrs = append(allErrs, ValidatePhaseTransition(oldSAC.Spec.Phase, newSAC.Spec.Phase, field.NewPath("spec", "phase"))...)

	if oldSAC.Spec.ServiceAgentRef.Name != newSAC.Spec.ServiceAgentRef.Name {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceAgentRef", "name"),
			"serviceAgentRef is immutable",
		))
	}

	if oldSAC.Spec.Phase == servicesv1alpha1.PhasePublished {
		allErrs = append(allErrs, validateAgentConfigurationPublishedImmutability(oldSAC, newSAC)...)
	}

	return allErrs
}

func validateAgentMetricUniqueness(sac *servicesv1alpha1.ServiceAgentConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	seen := make(map[string]struct{}, len(sac.Spec.Metrics))
	path := field.NewPath("spec", "metrics")
	for i, m := range sac.Spec.Metrics {
		if _, dup := seen[m.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(path.Index(i).Child("name"), m.Name))
		}
		seen[m.Name] = struct{}{}
	}
	return allErrs
}

// validateAgentMCPServers enforces MCP server name uniqueness and that
// every mutating tool is also present in the include allow-list — a
// tool that is never exposed cannot meaningfully be flagged as
// mutating.
func validateAgentMCPServers(sac *servicesv1alpha1.ServiceAgentConfiguration) field.ErrorList {
	var allErrs field.ErrorList
	if sac.Spec.Tools == nil {
		return nil
	}
	path := field.NewPath("spec", "tools", "mcpServers")
	seen := make(map[string]struct{}, len(sac.Spec.Tools.MCPServers))
	for i, srv := range sac.Spec.Tools.MCPServers {
		if _, dup := seen[srv.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(path.Index(i).Child("name"), srv.Name))
		}
		seen[srv.Name] = struct{}{}

		included := map[string]struct{}{}
		if srv.ToolSelector != nil {
			included = stringSet(srv.ToolSelector.Include)
		}
		for j, m := range srv.Mutating {
			if _, ok := included[m]; !ok {
				allErrs = append(allErrs, field.Invalid(
					path.Index(i).Child("mutating").Index(j), m,
					"must also appear in toolSelector.include",
				))
			}
		}
	}
	return allErrs
}

// validateServiceAgentConfigurationRefs resolves the referenced
// ServiceAgent and, through it, the owning Service, then enforces that
// every metric name is prefixed by the Service's canonical
// spec.serviceName — the same invariant ServiceConfiguration metrics
// carry.
func validateServiceAgentConfigurationRefs(
	ctx context.Context,
	c client.Reader,
	sac *servicesv1alpha1.ServiceAgentConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList
	agentRefPath := field.NewPath("spec", "serviceAgentRef", "name")

	if c == nil || sac.Spec.ServiceAgentRef.Name == "" {
		return allErrs
	}

	var agent servicesv1alpha1.ServiceAgent
	if err := c.Get(ctx, types.NamespacedName{Name: sac.Spec.ServiceAgentRef.Name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				agentRefPath, sac.Spec.ServiceAgentRef.Name,
				fmt.Sprintf("no ServiceAgent with metadata.name %q exists", sac.Spec.ServiceAgentRef.Name),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(agentRefPath,
			fmt.Errorf("failed to load referenced ServiceAgent: %w", err)))
		return allErrs
	}

	var svc servicesv1alpha1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: agent.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			// The agent's Service is gone; the metric prefix cannot be
			// resolved. Surface it on the agent ref rather than failing
			// open.
			allErrs = append(allErrs, field.Invalid(
				agentRefPath, sac.Spec.ServiceAgentRef.Name,
				fmt.Sprintf("referenced ServiceAgent's Service %q no longer exists", agent.Spec.ServiceRef.Name),
			))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(agentRefPath,
			fmt.Errorf("failed to load Service backing referenced ServiceAgent: %w", err)))
		return allErrs
	}

	canonical := svc.Spec.ServiceName
	if canonical == "" {
		return allErrs
	}
	prefix := canonical + "/"

	metricsPath := field.NewPath("spec", "metrics")
	for i, m := range sac.Spec.Metrics {
		if m.Name == "" {
			continue
		}
		if !strings.HasPrefix(m.Name, prefix) || strings.TrimPrefix(m.Name, prefix) == "" {
			allErrs = append(allErrs, field.Invalid(
				metricsPath.Index(i).Child("name"), m.Name,
				fmt.Sprintf("must be prefixed with the referenced service %q (e.g. %q)",
					prefix, prefix+"example-metric"),
			))
		}
	}
	return allErrs
}

// validateAgentConfigurationPublishedImmutability rejects changes to
// metric identity fields (kind, unit) and metric removal once the
// document is Published, mirroring the ServiceConfiguration rules.
// Knowledge, tools, delegation, and authority remain editable: the
// active-version selection model means consumers always follow the
// latest Published document.
func validateAgentConfigurationPublishedImmutability(
	oldSAC, newSAC *servicesv1alpha1.ServiceAgentConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	oldMetrics := make(map[string]servicesv1alpha1.MetricSpec, len(oldSAC.Spec.Metrics))
	for _, m := range oldSAC.Spec.Metrics {
		oldMetrics[m.Name] = m
	}
	newMetricSet := make(map[string]struct{}, len(newSAC.Spec.Metrics))
	for _, m := range newSAC.Spec.Metrics {
		newMetricSet[m.Name] = struct{}{}
	}
	metricsPath := field.NewPath("spec", "metrics")
	for name, old := range oldMetrics {
		if _, exists := newMetricSet[name]; !exists {
			allErrs = append(allErrs, field.Forbidden(metricsPath,
				fmt.Sprintf("cannot remove metric %q after publishing", name)))
			continue
		}
		for i, m := range newSAC.Spec.Metrics {
			if m.Name != name {
				continue
			}
			if m.Kind != old.Kind {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("kind"),
					"immutable after publishing"))
			}
			if m.Unit != old.Unit {
				allErrs = append(allErrs, field.Forbidden(metricsPath.Index(i).Child("unit"),
					"immutable after publishing"))
			}
		}
	}

	return allErrs
}
