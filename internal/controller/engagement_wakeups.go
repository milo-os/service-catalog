// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// engagementWakeups remembers requests waiting for a project connection. The
// multicluster manager calls Engage when that project becomes available; the
// synthetic event then places each dependent request back on its controller's
// queue without a polling interval. After an operator restart projects engage
// over tens of seconds, and exponential backoff would otherwise add as much
// again before a waiting request noticed.
//
// Callers register with waitFor before checking the cluster a second time, so
// an engagement that lands between the failed lookup and the registration is
// still observed.
type engagementWakeups struct {
	mu      sync.Mutex
	waiting map[multicluster.ClusterName]map[mcreconcile.Request]struct{}
	events  chan event.TypedGenericEvent[mcreconcile.Request]
}

func newEngagementWakeups() *engagementWakeups {
	return &engagementWakeups{
		waiting: make(map[multicluster.ClusterName]map[mcreconcile.Request]struct{}),
		events:  make(chan event.TypedGenericEvent[mcreconcile.Request], 1024),
	}
}

func (w *engagementWakeups) waitFor(clusterName multicluster.ClusterName, req mcreconcile.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiting[clusterName] == nil {
		w.waiting[clusterName] = make(map[mcreconcile.Request]struct{})
	}
	w.waiting[clusterName][req] = struct{}{}
}

func (w *engagementWakeups) forget(clusterName multicluster.ClusterName, req mcreconcile.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.waiting[clusterName], req)
	if len(w.waiting[clusterName]) == 0 {
		delete(w.waiting, clusterName)
	}
}

func (w *engagementWakeups) Engage(ctx context.Context, name multicluster.ClusterName, _ cluster.Cluster) error {
	w.mu.Lock()
	requests := w.waiting[name]
	delete(w.waiting, name)
	w.mu.Unlock()

	// Engage must remain non-blocking even when many requests depend on the
	// same project. If that project disengages before delivery completes,
	// retain the unsent requests for its next engagement.
	go func() {
		for req := range requests {
			select {
			case w.events <- event.TypedGenericEvent[mcreconcile.Request]{Object: req}:
				delete(requests, req)
			case <-ctx.Done():
				w.mu.Lock()
				if w.waiting[name] == nil {
					w.waiting[name] = make(map[mcreconcile.Request]struct{})
				}
				for pending := range requests {
					w.waiting[name][pending] = struct{}{}
				}
				w.mu.Unlock()
				return
			}
		}
	}()
	return nil
}

// cluster returns the named project cluster, or ok=false after arranging for
// req to be re-enqueued when that project engages. Any error other than the
// cluster not being engaged is returned as-is. A nil receiver only looks the
// cluster up, so reconcilers built without SetupWithManager still work.
func (w *engagementWakeups) cluster(ctx context.Context, mgr mcmanager.Manager, name multicluster.ClusterName, req mcreconcile.Request) (cluster.Cluster, bool, error) {
	cl, err := mgr.GetCluster(ctx, name)
	if err == nil {
		return cl, true, nil
	}
	if !errors.Is(err, multicluster.ErrClusterNotFound) {
		return nil, false, fmt.Errorf("failed to get cluster %q: %w", name, err)
	}
	if w == nil {
		return nil, false, nil
	}
	// Register before checking again so engagement cannot race between the
	// failed lookup and recording this dependency.
	w.waitFor(name, req)
	if cl, err = mgr.GetCluster(ctx, name); err == nil {
		w.forget(name, req)
		return cl, true, nil
	}
	return nil, false, nil
}

// source feeds woken requests to the controller that registered them.
func (w *engagementWakeups) source() source.TypedSource[mcreconcile.Request] {
	return source.TypedChannel(
		w.events,
		handler.TypedFuncs[mcreconcile.Request, mcreconcile.Request]{
			GenericFunc: func(_ context.Context, evt event.TypedGenericEvent[mcreconcile.Request], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
				q.Add(evt.Object)
			},
		},
	)
}

func (w *engagementWakeups) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (*engagementWakeups) NeedLeaderElection() bool { return false }

var _ mcmanager.Runnable = (*engagementWakeups)(nil)
