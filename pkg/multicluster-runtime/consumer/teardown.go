// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// labelServiceName is the cleanup label every object this operator projects into
// a consumer project carries, valued with the CANONICAL Service.spec.serviceName
// (e.g. "compute.miloapis.com"). It mirrors the catalog's LocationBindingReconciler,
// which stamps the same label (internal/controller/locationbinding_controller.go;
// that package is not importable from pkg/, so the constant is duplicated here).
//
// Teardown deletes are scoped to this label for the operator's ServiceNames — the
// same key the engagement active-set is counted on — so "delete everything
// labelled services.miloapis.com/service-name ∈ ServiceNames" matches exactly what
// engagement counted. Teardown NEVER deletes by the coarse
// app.kubernetes.io/managed-by label, which other operators in the same consumer
// project would share.
const labelServiceName = "services.miloapis.com/service-name"

// teardownConsumer removes everything this operator created in a consumer project
// when that project loses its last active ServiceConsumer. It is called from
// disengage AFTER the per-cluster context has been cancelled, so it builds a fresh
// NON-CACHED direct client (the engaged cluster's cache is gone) and talks to the
// consumer control plane directly.
//
// It deletes the declared ManagedResources scoped by labelServiceName, then runs
// the caller's Teardowns in declared order. Any error is returned so disengage can
// abort and leave the cluster tracked for a backoff retry. It is a no-op (and does
// not even build a client) when the operator declared neither ManagedResources nor
// Teardowns.
func (p *Provider) teardownConsumer(ctx context.Context, consumerProject string) error {
	if len(p.opts.ManagedResources) == 0 && len(p.opts.Teardowns) == 0 {
		return nil
	}

	cfg, err := p.consumerRestConfig(consumerProject)
	if err != nil {
		return err
	}
	direct, err := p.newClient(cfg, client.Options{Scheme: p.opts.Scheme})
	if err != nil {
		return fmt.Errorf("failed to build direct client for consumer project %q: %w", consumerProject, err)
	}

	if err := p.deleteManagedResources(ctx, direct, consumerProject); err != nil {
		return err
	}
	return p.runTeardowns(ctx, direct, consumerProject)
}

// deleteManagedResources deletes every object of each declared ManagedResources
// GVK that carries labelServiceName equal to one of the operator's ServiceNames.
// Objects are addressed as unstructured so the consumer cluster's RESTMapper
// resolves GVK→resource without the type being registered in a scheme.
//
// Cluster-scoped GVKs use a single DeleteAllOf. Namespace-scoped GVKs require
// per-namespace DeleteAllOf because Milo's aggregated API server does not support
// all-namespaces collection deletes (DELETE /apis/group/version/resources). The
// namespace list is fetched lazily and only when at least one namespace-scoped
// GVK is present.
//
// A missing type is treated as "nothing to clean up", not a failure: a RESTMapper
// no-match (the GVK's CRD is not installed in this consumer project) or a NotFound
// is tolerated and skipped.
func (p *Provider) deleteManagedResources(ctx context.Context, direct client.Client, consumerProject string) error {
	var nsList *unstructured.UnstructuredList

	for _, gvk := range p.opts.ManagedResources {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)

		namespaced, err := direct.IsObjectNamespaced(u)
		if err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("checking scope of %s in consumer project %q: %w", gvk.Kind, consumerProject, err)
		}

		if !namespaced {
			if err := p.deleteClusterScopedGVK(ctx, direct, gvk, consumerProject); err != nil {
				return err
			}
			continue
		}

		if nsList == nil {
			nsList = &unstructured.UnstructuredList{}
			nsList.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "NamespaceList"})
			if err := direct.List(ctx, nsList); err != nil {
				return fmt.Errorf("listing namespaces in consumer project %q: %w", consumerProject, err)
			}
		}
		if err := p.deleteNamespacedGVK(ctx, direct, gvk, nsList, consumerProject); err != nil {
			return err
		}
	}
	return nil
}

// deleteClusterScopedGVK issues a single label-scoped DeleteAllOf for a
// cluster-scoped GVK across all service names.
func (p *Provider) deleteClusterScopedGVK(ctx context.Context, direct client.Client, gvk schema.GroupVersionKind, consumerProject string) error {
	for _, serviceName := range p.opts.ServiceNames {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := direct.DeleteAllOf(ctx, u, client.MatchingLabels{labelServiceName: serviceName}); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to delete %s objects labelled %s=%s in consumer project %q: %w",
				gvk.Kind, labelServiceName, serviceName, consumerProject, err)
		}
	}
	return nil
}

// deleteNamespacedGVK issues a label-scoped DeleteAllOf per namespace for a
// namespace-scoped GVK across all service names.
func (p *Provider) deleteNamespacedGVK(ctx context.Context, direct client.Client, gvk schema.GroupVersionKind, nsList *unstructured.UnstructuredList, consumerProject string) error {
	for _, serviceName := range p.opts.ServiceNames {
		for _, ns := range nsList.Items {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)
			if err := direct.DeleteAllOf(ctx, u,
				client.InNamespace(ns.GetName()),
				client.MatchingLabels{labelServiceName: serviceName},
			); err != nil {
				if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("failed to delete %s objects labelled %s=%s in namespace %s of consumer project %q: %w",
					gvk.Kind, labelServiceName, serviceName, ns.GetName(), consumerProject, err)
			}
		}
	}
	return nil
}

// runTeardowns runs each caller-supplied Teardown in declared order against the
// direct (non-cached) client. The first error aborts; each Teardown must be
// idempotent so a retry after a later-hook failure is safe.
func (p *Provider) runTeardowns(ctx context.Context, direct client.Client, consumerProject string) error {
	for _, td := range p.opts.Teardowns {
		if err := td.TeardownConsumer(ctx, consumerProject, direct, p.opts.ServiceNames); err != nil {
			return fmt.Errorf("teardown failed for consumer project %q: %w", consumerProject, err)
		}
	}
	return nil
}

// runSuspends runs each caller-supplied Suspend in declared order against the
// direct (non-cached) client. The first error aborts; each Suspend must be
// idempotent so a retry after a later-hook failure is safe.
func (p *Provider) runSuspends(ctx context.Context, direct client.Client, consumerProject string) error {
	for _, s := range p.opts.Suspends {
		if err := s.SuspendConsumer(ctx, consumerProject, direct, p.opts.ServiceNames); err != nil {
			return fmt.Errorf("suspend failed for consumer project %q: %w", consumerProject, err)
		}
	}
	return nil
}

// runResumes runs each caller-supplied Resume in declared order against the
// direct (non-cached) client. The first error aborts; each Resume must be
// idempotent so a retry after a later-hook failure is safe.
func (p *Provider) runResumes(ctx context.Context, direct client.Client, consumerProject string) error {
	for _, r := range p.opts.Resumes {
		if err := r.ResumeConsumer(ctx, consumerProject, direct, p.opts.ServiceNames); err != nil {
			return fmt.Errorf("resume failed for consumer project %q: %w", consumerProject, err)
		}
	}
	return nil
}
