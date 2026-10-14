// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// blockingMCMgr holds Engage open until release is closed, so a test can act
// while an engagement is in progress.
type blockingMCMgr struct {
	engaging chan struct{}
	release  chan struct{}
}

func (m *blockingMCMgr) Engage(context.Context, multicluster.ClusterName, cluster.Cluster) error {
	close(m.engaging)
	<-m.release
	return nil
}

// Get must not wait on an in-progress Engage. Engage blocks on any controller
// that is still starting, and a starting controller's event handlers may call
// Get; if Get waited on Engage the two would deadlock (milo-os/milo#840). The
// project being engaged must also already resolve, so a reconcile triggered
// during Engage is not dropped as cluster-not-found.
func TestEngage_GetDoesNotBlockWhileEngaging(t *testing.T) {
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
	}
	mcMgr := &blockingMCMgr{engaging: make(chan struct{}), release: make(chan struct{})}
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical}, withMCMgr(mcMgr))

	reconciled := make(chan error, 1)
	go func() {
		_, err := p.Reconcile(context.Background(), ctrl.Request{})
		reconciled <- err
	}()

	select {
	case <-mcMgr.engaging:
	case <-time.After(10 * time.Second):
		t.Fatal("Reconcile never reached Engage")
	}

	type result struct {
		cl  cluster.Cluster
		err error
	}
	got := make(chan result, 1)
	go func() {
		cl, err := p.Get(context.Background(), "proj-a")
		got <- result{cl, err}
	}()

	select {
	case r := <-got:
		if r.err != nil || r.cl == nil {
			t.Errorf("Get during Engage = (%v, %v), want the cluster being engaged", r.cl, r.err)
		}
	case <-time.After(2 * time.Second):
		close(mcMgr.release)
		t.Fatal("Get blocked while a consumer project was being engaged")
	}

	close(mcMgr.release)
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestMetrics_DesiredClustersGauge(t *testing.T) {
	objs := []client.Object{
		newService(computeObject, computeCanonical),
		newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
		newConsumer("c-proj-b", computeObject, "proj-b", servicesv1alpha1.ConsumerPhaseActive),
		newConsumer("c-proj-c", computeObject, "proj-c", servicesv1alpha1.ConsumerPhasePendingApproval),
	}
	p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical}, withMCMgr(newFakeMCMgr()))

	if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readMetric(t, desiredClusters).GetGauge().GetValue(); got != 2 {
		t.Errorf("desired-clusters gauge = %v, want 2 active consumer projects", got)
	}
}

func TestMetrics_EngageFailureCounter(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		opt   providerOpt
		mgr   func() *fakeMCMgr
	}{
		{
			name:  "engage",
			stage: stageEngage,
			mgr: func() *fakeMCMgr {
				m := newFakeMCMgr()
				m.engageErr = errors.New("engage boom")
				return m
			},
		},
		{
			name:  "cache sync",
			stage: stageCacheSync,
			opt: withNewCluster(func(*rest.Config, ...cluster.Option) (cluster.Cluster, error) {
				return &fakeCluster{cache: &fakeCache{failSync: true}}, nil
			}),
			mgr: newFakeMCMgr,
		},
		{
			name:  "create cluster",
			stage: stageCreateCluster,
			opt: withNewCluster(func(*rest.Config, ...cluster.Option) (cluster.Cluster, error) {
				return nil, errors.New("create boom")
			}),
			mgr: newFakeMCMgr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{
				newService(computeObject, computeCanonical),
				newConsumer("c-proj-a", computeObject, "proj-a", servicesv1alpha1.ConsumerPhaseActive),
			}
			opts := []providerOpt{withMCMgr(tc.mgr())}
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			p := newTestProvider(newProviderClient(t, objs...), []string{computeCanonical}, opts...)
			counter := engageFailuresTotal.WithLabelValues(tc.stage)
			before := counterValue(t, counter)

			if _, err := p.Reconcile(context.Background(), ctrl.Request{}); err == nil {
				t.Fatalf("expected reconcile to fail at the %s stage", tc.stage)
			}
			if got := counterValue(t, counter) - before; got != 1 {
				t.Errorf("engage-failure counter for stage %q rose by %v, want 1", tc.stage, got)
			}
		})
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	return readMetric(t, c).GetCounter().GetValue()
}
