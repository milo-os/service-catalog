// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"
)

// mcProvider is the subset of the Milo and e2esingle providers' capabilities
// main needs: resolving cluster names (multicluster.Provider) and running the
// provider's background engagement loop (multicluster.ProviderRunnable).
type mcProvider interface {
	multicluster.Provider
	multicluster.ProviderRunnable
}

// newDiscoveryManager builds the manager that hosts project discovery and
// cluster engagement.
//
// It deliberately runs no leader election. Providers register their discovery
// reconciler on the manager they are handed, and controller-runtime
// leader-gates reconcilers by default, so hosting the provider on the primary
// manager left non-leader replicas with no engaged clusters at all — and the
// ServiceConsumer webhook, which resolves the caller's project through them
// and fails closed, denied every request those replicas served (#62).
// Engagement is per-pod cache and watch bookkeeping, the same category of
// thing as the manager's own informer cache, which controller-runtime never
// leader-gates.
func newDiscoveryManager(cfg *rest.Config) (manager.Manager, error) {
	return ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			// Avoid port conflict with the primary manager's metrics server.
			BindAddress: "0",
		},
	})
}

// newMulticlusterManager wraps localMgr as a multicluster manager backed by
// provider. localMgr must be a manager that runs no leader election, and it
// also starts provider — callers must not start it themselves.
func newMulticlusterManager(localMgr manager.Manager, provider mcProvider) (mcmanager.Manager, error) {
	// mcManager.Start auto-wires a ProviderRunnable's Start as a plain
	// manager.RunnableFunc, which implements no NeedLeaderElection, so
	// controller-runtime's runnable-group router sends it into the
	// leader-election group. Milo's guard hides Start from that auto-wiring
	// and re-adds it as an always-running runnable. e2esingle carries no such
	// guard and needs none: it is only ever wired up by the e2e suite.
	if p, ok := provider.(*miloprovider.Provider); ok {
		mcMgr, err := mcmanager.WithMultiCluster(localMgr, miloprovider.WithoutAutoStart(p))
		if err != nil {
			return nil, err
		}
		return mcMgr, miloprovider.EngageAlways(mcMgr, p)
	}

	return mcmanager.WithMultiCluster(localMgr, provider)
}
