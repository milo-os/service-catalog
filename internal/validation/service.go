// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// serviceSlugRegex constrains metadata.name for Service resources to a
// Kubernetes DNS-1123 label style: lowercase alphanumerics and hyphens,
// must start and end with an alphanumeric, up to 63 chars.
var serviceSlugRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// reverseDNSRegex matches a reverse-DNS identifier such as
// "compute.miloapis.com": two or more lowercase DNS labels joined by
// dots. Individual labels are up to 63 chars and the whole string up to
// 253 (enforced separately via the CRD MaxLength).
var reverseDNSRegex = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`,
)

// ValidateServiceCreate validates a Service on creation.
func ValidateServiceCreate(svc *servicesv1alpha1.Service) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateServiceMetadataName(svc)...)
	allErrs = append(allErrs, validateServiceName(svc.Spec.ServiceName)...)

	return allErrs
}

// ValidateServiceUpdate validates a Service on update. CRD-level CEL
// already enforces serviceName immutability; the belt-and-suspenders
// check keeps the invariant in sync with validation tests.
func ValidateServiceUpdate(oldSvc, newSvc *servicesv1alpha1.Service) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateServiceMetadataName(newSvc)...)
	allErrs = append(allErrs, validateServiceName(newSvc.Spec.ServiceName)...)

	if oldSvc.Spec.ServiceName != newSvc.Spec.ServiceName {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "serviceName"),
			"serviceName is immutable",
		))
	}

	allErrs = append(allErrs, ValidatePhaseTransition(
		oldSvc.Spec.Phase, newSvc.Spec.Phase,
		field.NewPath("spec", "phase"),
	)...)

	return allErrs
}

// ValidateServiceDependencies fetches every Service in the catalog,
// builds a directed graph of metadata.name dependencies, and rejects the
// admission if introducing svc would create a cycle. Returns a populated
// ErrorList with the cycle path when one is found.
func ValidateServiceDependencies(
	ctx context.Context,
	c client.Reader,
	svc *servicesv1alpha1.Service,
) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "dependencies")

	if c == nil || len(svc.Spec.Dependencies) == 0 {
		return allErrs
	}

	var list servicesv1alpha1.ServiceList
	if err := c.List(ctx, &list); err != nil {
		allErrs = append(allErrs, field.InternalError(fldPath,
			fmt.Errorf("failed to list Services for dependency cycle detection: %w", err)))
		return allErrs
	}

	// Adjacency by metadata.name; substitute the admitted spec for the
	// existing object so the check reflects the post-admission graph.
	adj := make(map[string][]string, len(list.Items)+1)
	for _, s := range list.Items {
		if s.Name == svc.Name {
			continue
		}
		adj[s.Name] = dependencyNames(s.Spec.Dependencies)
	}
	adj[svc.Name] = dependencyNames(svc.Spec.Dependencies)

	if path := findDependencyCycle(svc.Name, adj); len(path) > 0 {
		allErrs = append(allErrs, field.Invalid(
			fldPath, dependencyNames(svc.Spec.Dependencies),
			fmt.Sprintf("dependency cycle detected: %s", strings.Join(path, " -> ")),
		))
	}
	return allErrs
}

func dependencyNames(deps []servicesv1alpha1.ServiceDependency) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if d.ServiceRef.Name != "" {
			out = append(out, d.ServiceRef.Name)
		}
	}
	return out
}

// findDependencyCycle runs a DFS coloring from start. Colors: 0=unseen,
// 1=on-stack, 2=done. Returns the cycle path (start..repeated node)
// when a back-edge is found, otherwise an empty slice.
func findDependencyCycle(start string, adj map[string][]string) []string {
	color := make(map[string]int, len(adj))
	stack := make([]string, 0, len(adj))

	var dfs func(node string) []string
	dfs = func(node string) []string {
		color[node] = 1
		stack = append(stack, node)
		for _, next := range adj[node] {
			switch color[next] {
			case 0:
				if cycle := dfs(next); len(cycle) > 0 {
					return cycle
				}
			case 1:
				// Back-edge — trim the stack to the start of the cycle.
				cycle := make([]string, 0, len(stack)+1)
				started := false
				for _, n := range stack {
					if n == next {
						started = true
					}
					if started {
						cycle = append(cycle, n)
					}
				}
				cycle = append(cycle, next)
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = 2
		return nil
	}

	return dfs(start)
}

func validateServiceMetadataName(svc *servicesv1alpha1.Service) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("metadata", "name")

	name := svc.GetName()
	if name == "" {
		// Let the apiserver handle required-name errors; nothing to do here.
		return allErrs
	}
	if len(name) > 63 {
		allErrs = append(allErrs, field.Invalid(
			fldPath, name,
			"must be 63 characters or fewer",
		))
		return allErrs
	}
	if !serviceSlugRegex.MatchString(name) {
		allErrs = append(allErrs, field.Invalid(
			fldPath, name,
			"must be a DNS-1123 label (lowercase alphanumerics and hyphens, must start and end with an alphanumeric)",
		))
	}
	return allErrs
}

// validateServiceName enforces reverse-DNS shape for spec.serviceName.
// The CRD already enforces length bounds; this covers the format.
func validateServiceName(serviceName string) field.ErrorList {
	var allErrs field.ErrorList
	fldPath := field.NewPath("spec", "serviceName")

	if serviceName == "" {
		// Required field — CRD will reject; skip to avoid double error.
		return allErrs
	}
	if !reverseDNSRegex.MatchString(serviceName) {
		allErrs = append(allErrs, field.Invalid(
			fldPath, serviceName,
			fmt.Sprintf("must be a reverse-DNS name such as %q", "compute.miloapis.com"),
		))
	}
	return allErrs
}
