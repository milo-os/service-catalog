// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
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
// cluster engagement for admission. Controllers that write to project planes
// do not belong on it — everything it hosts runs on every replica; they go on
// newElectedMulticlusterManager instead.
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
func newDiscoveryManager(cfg *rest.Config, controllerOpts ctrlconfig.Controller) (manager.Manager, error) {
	return ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     scheme,
		Controller: controllerOpts,
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

// electedLeaseID is the lease that decides which replica runs the mutating
// project controllers. It keeps the name it had when only Location projection
// lived behind it, so a rolling upgrade hands the same lease from old pods to
// new ones instead of briefly electing two holders under different names.
const electedLeaseID = "project-projection.services.miloapis.com"

// newElectedMulticlusterManager builds the multicluster manager that hosts the
// project controllers that write: ServiceEntitlement, ServiceActivationRequest,
// ServiceConsumer, Provisioning and, unless consumer-scoped projection is on,
// Location projection and CapabilityBinding.
//
// It is the counterpart to newDiscoveryManager. Admission needs engaged
// project clusters on every replica, but these reconcilers must not run on
// every replica: two pods reconciling the same object race on
// resourceVersion and back off on every conflict. So they get their own
// local manager under leaderElection and their own Milo provider, whose
// discovery reconciler and engagement loop are both leader-gated — a
// non-leader starts no caches, engages nothing and runs no workers here.
// leaderElection carries only the leader-election fields; everything else is
// fixed.
func newElectedMulticlusterManager(cfg *rest.Config, controllerOpts ctrlconfig.Controller, leaderElection ctrl.Options) (mcmanager.Manager, error) {
	localMgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Controller:              controllerOpts,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          leaderElection.LeaderElection,
		LeaderElectionID:        electedLeaseID,
		LeaderElectionNamespace: leaderElection.LeaderElectionNamespace,
		LeaseDuration:           leaderElection.LeaseDuration,
		RenewDeadline:           leaderElection.RenewDeadline,
		RetryPeriod:             leaderElection.RetryPeriod,
	})
	if err != nil {
		return nil, err
	}
	provider, err := newMiloProvider(localMgr, miloprovider.Options{
		InternalServiceDiscovery: false,
		ProjectRestConfig:        cfg,
		ClusterOptions:           []cluster.Option{func(o *cluster.Options) { o.Scheme = scheme }},
	})
	if err != nil {
		return nil, err
	}
	return newLeaderGatedMulticlusterManager(localMgr, provider)
}

// newLeaderGatedMulticlusterManager wraps localMgr as a multicluster manager
// backed by provider, and registers the provider's engagement loop on localMgr
// as a plain runnable. controller-runtime routes that into the leader-election
// group, so engagement waits for the lease like every controller added to the
// returned manager. This is newMulticlusterManager's routing inverted: there
// EngageAlways deliberately keeps engagement out of that group.
func newLeaderGatedMulticlusterManager(localMgr manager.Manager, provider *miloprovider.Provider) (mcmanager.Manager, error) {
	mcMgr, err := mcmanager.WithMultiCluster(localMgr, miloprovider.WithoutAutoStart(provider))
	if err != nil {
		return nil, err
	}
	return mcMgr, localMgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return provider.Start(ctx, mcMgr)
	}))
}
