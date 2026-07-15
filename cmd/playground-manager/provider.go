// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

var _ multicluster.Provider = &staticProvider{}

// staticProvider serves one underlying cluster.Cluster under several names.
//
// The watchName is engaged on the multicluster manager, so mcbuilder-based
// controllers receive events with a non-empty, resolvable ClusterName. The
// alias names are resolvable via Get only — reconcilers that look up other
// "project control planes" (e.g. the provider project during entitlement
// activation) transparently land on the same envtest apiserver. Aliases are
// intentionally not engaged: engaging the same backing store twice would
// double-deliver every watch event.
//
// Modeled on multicluster-runtime's providers/single.
type staticProvider struct {
	watchName string
	names     map[string]struct{}
	cl        cluster.Cluster
}

func newStaticProvider(watchName string, cl cluster.Cluster, aliases ...string) *staticProvider {
	names := map[string]struct{}{watchName: {}}
	for _, a := range aliases {
		names[a] = struct{}{}
	}
	return &staticProvider{watchName: watchName, names: names, cl: cl}
}

// Run starts the underlying cluster, waits for its cache, engages it under
// watchName, and blocks until the context is cancelled.
func (p *staticProvider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	errCh := make(chan error, 1)
	go func() {
		if err := p.cl.Start(ctx); err != nil {
			errCh <- err
		}
	}()

	if !p.cl.GetCache().WaitForCacheSync(ctx) {
		select {
		case err := <-errCh:
			return err
		default:
			return errors.New("static provider: cache did not sync")
		}
	}

	if err := mgr.Engage(ctx, p.watchName, p.cl); err != nil {
		return err
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return nil
	}
}

// Get resolves the cluster under its watch name or any alias.
func (p *staticProvider) Get(_ context.Context, clusterName string) (cluster.Cluster, error) {
	if _, ok := p.names[clusterName]; ok {
		return p.cl, nil
	}
	return nil, multicluster.ErrClusterNotFound
}

// IndexField indexes on the single underlying cluster.
func (p *staticProvider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	return p.cl.GetFieldIndexer().IndexField(ctx, obj, field, extractValue)
}
