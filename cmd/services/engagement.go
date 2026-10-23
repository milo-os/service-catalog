// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/pkg/multicluster-runtime/prewarm"
)

// defaultProjectEngagementWorkers is the default for
// --project-engagement-workers, which bounds how many project control planes
// a Milo provider connects to at once. The provider's own controller engages
// one project at a time, so a freshly started operator spent well over a
// minute working through every project before it could serve the last one.
// Each engagement is dominated by round trips to that project's control
// plane, so running them side by side brings the whole pass down to seconds.
//
// Each worker lists every kind its manager's controllers watch in one
// project, so this also caps how many of those lists the operator has in
// flight against Milo at startup. Eight keeps that burst modest while still
// connecting a few hundred projects in well under a minute.
const defaultProjectEngagementWorkers = 8

// projectEngagementWorkers is set from --project-engagement-workers.
var projectEngagementWorkers = defaultProjectEngagementWorkers

// providerProjectPriority queues projects that own a Service ahead of the
// initial list of every other project. Entitlements in any consumer project
// wait on their service's provider project, so engaging providers first
// unblocks setup for everyone. Projects created after startup already get the
// queue's default priority, ahead of the low-priority initial list.
const providerProjectPriority = 100

// providerListRetryInterval spaces out attempts to list Services when the
// first attempt fails, so a struggling API server is not asked once per
// project in the initial list.
const providerListRetryInterval = 5 * time.Second

// newMiloProvider builds a Milo provider whose project discovery runs on
// projectEngagementWorkers workers and engages provider projects first. It
// otherwise behaves exactly like miloprovider.New: the discovery controller is
// registered on localMgr and runs whenever localMgr runs its controllers.
//
// The provider's own discovery controller is fixed at one worker, so New is
// handed a manager that keeps that controller from being added, and the
// provider's exported Reconcile is driven by a controller built here instead.
// Running Reconcile for different projects in parallel is safe: the provider
// guards its state with a lock, and the workqueue never hands the same project
// to two workers at once.
func newMiloProvider(localMgr manager.Manager, opts miloprovider.Options) (*miloprovider.Provider, error) {
	if opts.InternalServiceDiscovery || opts.LabelSelector != nil {
		return nil, fmt.Errorf("newMiloProvider supports Project discovery without a label selector only")
	}

	// Each provider learns the kinds its own controllers watch; see
	// prewarm.WatchedKinds.
	opts.ClusterOptions = append(append([]cluster.Option(nil), opts.ClusterOptions...), prewarm.NewWatchedKinds().ClusterOption())

	withheld := &controllerWithholdingManager{Manager: localMgr}
	provider, err := miloprovider.New(withheld, opts)
	if err != nil {
		return nil, err
	}
	if withheld.withheld != 1 {
		return nil, fmt.Errorf("expected the Milo provider to register one discovery controller, got %d", withheld.withheld)
	}

	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(resourcemanagerv1alpha1.GroupVersion.WithKind("Project"))

	err = builder.ControllerManagedBy(localMgr).
		// Keep the provider's controller name so its workqueue metrics carry
		// on under the same label. Both project managers in this process use
		// it, so skip controller-runtime's process-wide uniqueness check.
		Named("projectcontrolplane").
		WithOptions(controller.Options{
			MaxConcurrentReconciles: projectEngagementWorkers,
			SkipNameValidation:      ptr.To(true),
		}).
		Watches(project, &projectEnqueuer{
			isProvider: (&providerProjects{reader: localMgr.GetAPIReader()}).has,
		}).
		Complete(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to create project engagement controller: %w", err)
	}
	return provider, nil
}

// controllerWithholdingManager drops every controller added to it and passes
// all other runnables through. It also lets controllers built against it skip
// name validation, so a withheld controller never claims a name.
type controllerWithholdingManager struct {
	manager.Manager
	withheld int
}

func (m *controllerWithholdingManager) Add(r manager.Runnable) error {
	if _, ok := r.(controller.Controller); ok {
		m.withheld++
		return nil
	}
	return m.Manager.Add(r)
}

func (m *controllerWithholdingManager) GetControllerOptions() ctrlconfig.Controller {
	opts := m.Manager.GetControllerOptions()
	opts.SkipNameValidation = ptr.To(true)
	return opts
}

// projectEnqueuer enqueues Project events like the provider's own controller
// does, except that projects from the initial list which own a Service jump
// ahead of the rest.
type projectEnqueuer struct {
	isProvider func(ctx context.Context, name string) bool
}

var defaultProjectHandler = handler.WithLowPriorityWhenUnchanged(&handler.EnqueueRequestForObject{})

func (e *projectEnqueuer) Create(ctx context.Context, evt event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if pq, ok := q.(priorityqueue.PriorityQueue[reconcile.Request]); ok && evt.IsInInitialList && e.isProvider(ctx, evt.Object.GetName()) {
		pq.AddWithOpts(priorityqueue.AddOpts{Priority: ptr.To(providerProjectPriority)},
			reconcile.Request{NamespacedName: client.ObjectKeyFromObject(evt.Object)})
		return
	}
	defaultProjectHandler.Create(ctx, evt, q)
}

func (e *projectEnqueuer) Update(ctx context.Context, evt event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	defaultProjectHandler.Update(ctx, evt, q)
}

func (e *projectEnqueuer) Delete(ctx context.Context, evt event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	defaultProjectHandler.Delete(ctx, evt, q)
}

func (e *projectEnqueuer) Generic(ctx context.Context, evt event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	defaultProjectHandler.Generic(ctx, evt, q)
}

// providerProjects answers whether a project owns a Service. It lists Services
// once, on first use, which is while the initial project list is being queued;
// providers only matter for ordering that list.
type providerProjects struct {
	reader client.Reader

	mu          sync.Mutex
	names       map[string]struct{}
	lastAttempt time.Time
}

func (p *providerProjects) has(ctx context.Context, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.names == nil && time.Since(p.lastAttempt) >= providerListRetryInterval {
		p.lastAttempt = time.Now()
		var services servicesv1alpha1.ServiceList
		if err := p.reader.List(ctx, &services); err != nil {
			logf.FromContext(ctx).Error(err, "unable to list Services; engaging projects without provider priority")
		} else {
			p.names = make(map[string]struct{}, len(services.Items))
			for _, svc := range services.Items {
				if owner := svc.Spec.Owner.ProducerProjectRef.Name; owner != "" {
					p.names[owner] = struct{}{}
				}
			}
		}
	}

	_, ok := p.names[name]
	return ok
}
