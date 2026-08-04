// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"
)

const testClusterName = multicluster.ClusterName("test-project")

// replicaManager stands in for a controller-runtime manager running on a
// replica that never wins leader election. Start applies the same routing
// rule as controller-runtime's runnable group: when leader election is on,
// anything that is not explicitly a non-leader-elected runnable — including
// any plain manager.RunnableFunc, which implements no NeedLeaderElection at
// all — is held back for the leader and so never runs here.
type replicaManager struct {
	manager.Manager

	leaderElection bool

	mu        sync.Mutex
	runnables []manager.Runnable
}

func (m *replicaManager) Add(r manager.Runnable) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runnables = append(m.runnables, r)
	return nil
}

func (m *replicaManager) Start(ctx context.Context) error {
	for _, r := range m.added() {
		if m.leaderGated(r) {
			continue
		}
		go func(r manager.Runnable) { _ = r.Start(ctx) }(r)
	}

	<-ctx.Done()
	return nil
}

func (m *replicaManager) leaderGated(r manager.Runnable) bool {
	if !m.leaderElection {
		return false
	}
	le, ok := r.(manager.LeaderElectionRunnable)
	return !ok || le.NeedLeaderElection()
}

func (m *replicaManager) added() []manager.Runnable {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]manager.Runnable(nil), m.runnables...)
}

// stubProvider mimics the shape of the Milo provider: a discovery reconciler
// hosted on some manager engages clusters, but only once Start has handed it
// the multicluster.Aware to engage them with.
type stubProvider struct {
	mu       sync.Mutex
	aware    multicluster.Aware
	clusters map[multicluster.ClusterName]cluster.Cluster
}

func newStubProvider() *stubProvider {
	return &stubProvider{clusters: map[multicluster.ClusterName]cluster.Cluster{}}
}

// hostOn registers the discovery loop on mgr the way providers register their
// reconciler: as a runnable controller-runtime leader-gates by default.
func (p *stubProvider) hostOn(mgr manager.Manager) error {
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		for !p.discover(ctx) {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Millisecond):
			}
		}
		<-ctx.Done()
		return nil
	}))
}

func (p *stubProvider) discover(ctx context.Context) bool {
	p.mu.Lock()
	aware := p.aware
	p.mu.Unlock()
	if aware == nil {
		return false
	}

	cl := &stubCluster{}
	if err := aware.Engage(ctx, testClusterName, cl); err != nil {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.clusters[testClusterName] = cl
	return true
}

func (p *stubProvider) Start(ctx context.Context, aware multicluster.Aware) error {
	p.mu.Lock()
	p.aware = aware
	p.mu.Unlock()

	<-ctx.Done()
	return ctx.Err()
}

func (p *stubProvider) started() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aware != nil
}

func (p *stubProvider) Get(_ context.Context, name multicluster.ClusterName) (cluster.Cluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cl, ok := p.clusters[name]; ok {
		return cl, nil
	}
	return nil, fmt.Errorf("cluster %s not found", name)
}

func (p *stubProvider) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return nil
}

type stubCluster struct {
	cluster.Cluster
}

func eventuallyResolves(mcMgr mcmanager.Manager) bool {
	for range 200 {
		if _, err := mcMgr.GetCluster(context.Background(), testClusterName); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestNonLeaderReplicaResolvesClusters is the regression test for #62: a
// replica that never wins leader election must still resolve project
// clusters, because the ServiceConsumer webhook denies every request for a
// cluster it cannot resolve and the webhook Service routes to any pod.
func TestNonLeaderReplicaResolvesClusters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	primary := &replicaManager{leaderElection: true}
	discovery := &replicaManager{}

	provider := newStubProvider()
	if err := provider.hostOn(discovery); err != nil {
		t.Fatalf("hosting provider: %v", err)
	}

	mcMgr, err := newMulticlusterManager(discovery, provider)
	if err != nil {
		t.Fatalf("newMulticlusterManager: %v", err)
	}

	go func() { _ = primary.Start(ctx) }()
	go func() { _ = mcMgr.Start(ctx) }()

	if !eventuallyResolves(mcMgr) {
		t.Fatal("cluster never resolved on a non-leader replica")
	}
}

// TestLeaderElectedHostStrandsNonLeaderReplica pins the failure mode observed
// in production: hosting the provider's discovery loop on the leader-elected
// manager leaves the cluster unresolvable on every replica but the leader,
// even though the provider's engagement loop is running there.
func TestLeaderElectedHostStrandsNonLeaderReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	primary := &replicaManager{leaderElection: true}
	discovery := &replicaManager{}

	provider := newStubProvider()
	if err := provider.hostOn(primary); err != nil {
		t.Fatalf("hosting provider: %v", err)
	}

	mcMgr, err := newMulticlusterManager(discovery, provider)
	if err != nil {
		t.Fatalf("newMulticlusterManager: %v", err)
	}

	go func() { _ = primary.Start(ctx) }()
	go func() { _ = mcMgr.Start(ctx) }()

	if eventuallyResolves(mcMgr) {
		t.Fatal("cluster resolved although discovery was leader-gated; the fake no longer models controller-runtime's routing")
	}
	if !provider.started() {
		t.Fatal("provider engagement loop never ran on the non-leader replica")
	}
}

// TestMiloProviderEngagementIsNotAutoWired covers the second gate: left to
// itself, mcManager.Start auto-wires a ProviderRunnable's Start as a plain
// manager.RunnableFunc, which controller-runtime routes into the
// leader-election group. newMulticlusterManager must instead hand the Milo
// provider to milo's WithoutAutoStart/EngageAlways guard, which registers it
// as always-running.
func TestMiloProviderEngagementIsNotAutoWired(t *testing.T) {
	hostMgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("host manager: %v", err)
	}

	provider, err := miloprovider.New(hostMgr, miloprovider.Options{})
	if err != nil {
		t.Fatalf("milo provider: %v", err)
	}

	local := &replicaManager{leaderElection: true}
	mcMgr, err := newMulticlusterManager(local, provider)
	if err != nil {
		t.Fatalf("newMulticlusterManager: %v", err)
	}

	if _, autoWired := mcMgr.GetProvider().(multicluster.ProviderRunnable); autoWired {
		t.Error("provider is still a ProviderRunnable: mcManager.Start will auto-wire and leader-gate its Start")
	}

	var engagement bool
	for _, r := range local.added() {
		if le, ok := r.(manager.LeaderElectionRunnable); ok && !le.NeedLeaderElection() {
			engagement = true
		}
	}
	if !engagement {
		t.Error("no always-running engagement runnable was registered on the local manager")
	}
}
