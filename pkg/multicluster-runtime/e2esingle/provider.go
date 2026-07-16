// SPDX-License-Identifier: AGPL-3.0-only

// Package e2esingle provides a multicluster.Provider for e2e testing only.
// ServiceEntitlement, ServiceConsumer, and ProjectSuspensionPropagation are
// written against multicluster-runtime to address separate project virtual
// control planes in production. The e2e chainsaw cluster has no real Milo
// control plane to provide that (see config/overlays/e2e/config.yaml), so
// this Provider engages a single real cluster and resolves every requested
// cluster name — consumer project, provider project, whatever — to it. That
// collapses the production isolation between projects entirely, so it must
// never be wired in outside of e2e.
package e2esingle

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// ClusterName is the fixed name the single real cluster is engaged under.
// Fixtures exercising the entitlement/consumer/suspension flow under this
// provider must name their consumer Project object ClusterName: the
// entitlement controller's req.ClusterName comes from this engagement, and
// ProjectSuspensionPropagationReconciler derives the same consumer project
// name from the Project object it reads directly — the two must match for a
// ServiceConsumer's computed name to line up between them.
const ClusterName multicluster.ClusterName = "e2e-single-cluster"

var (
	_ multicluster.Provider         = &Provider{}
	_ multicluster.ProviderRunnable = &Provider{}
)

// Provider always resolves to the one cluster it was built with, regardless
// of which cluster name is requested.
type Provider struct {
	cl cluster.Cluster
}

// New returns a Provider that engages cl under ClusterName once Start runs.
func New(cl cluster.Cluster) *Provider {
	return &Provider{cl: cl}
}

// Start engages the single cluster once and blocks until ctx is done.
func (p *Provider) Start(ctx context.Context, aware multicluster.Aware) error {
	if err := aware.Engage(ctx, ClusterName, p.cl); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// Get always returns the single engaged cluster; the requested name is
// ignored on purpose (see package doc).
func (p *Provider) Get(context.Context, multicluster.ClusterName) (cluster.Cluster, error) {
	return p.cl, nil
}

// IndexField forwards the index request to the single cluster.
func (p *Provider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	return p.cl.GetFieldIndexer().IndexField(ctx, obj, field, extractValue)
}
