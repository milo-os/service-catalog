// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// recordingManager records what is added to it instead of running it.
type recordingManager struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return nil
}

// newMiloProvider must leave exactly one project discovery controller on the
// manager — its own, running several workers — rather than the provider's
// single-worker controller, or a fresh operator engages projects one by one.
func TestNewMiloProviderEngagesProjectsConcurrently(t *testing.T) {
	hostMgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("host manager: %v", err)
	}
	local := &recordingManager{Manager: hostMgr}

	if _, err := newMiloProvider(local, miloprovider.Options{}); err != nil {
		t.Fatalf("newMiloProvider: %v", err)
	}

	var controllers []controller.Controller
	for _, r := range local.added {
		if c, ok := r.(controller.Controller); ok {
			controllers = append(controllers, c)
		}
	}
	if len(controllers) != 1 {
		t.Fatalf("got %d project discovery controllers, want 1", len(controllers))
	}
	workers := reflect.ValueOf(controllers[0]).Elem().FieldByName("MaxConcurrentReconciles")
	if !workers.IsValid() {
		t.Fatal("controller no longer exposes MaxConcurrentReconciles; update this test")
	}
	if got := workers.Int(); got != projectEngagementWorkers {
		t.Errorf("MaxConcurrentReconciles = %d, want %d", got, projectEngagementWorkers)
	}
}

func projectCreate(name string) event.CreateEvent {
	return event.CreateEvent{
		Object:          &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}},
		IsInInitialList: true,
	}
}

// Provider projects in the initial list must be engaged before the consumer
// projects listed ahead of them.
func TestProjectEnqueuerEngagesProvidersFirst(t *testing.T) {
	q := priorityqueue.New[reconcile.Request]("test")
	defer q.ShutDown()
	e := &projectEnqueuer{isProvider: func(_ context.Context, name string) bool { return name == "datum-cloud" }}

	ctx := context.Background()
	for _, name := range []string{"aaa-consumer", "bbb-consumer", "datum-cloud", "zzz-consumer"} {
		e.Create(ctx, projectCreate(name), q)
	}

	first, _ := q.Get()
	if first.Name != "datum-cloud" {
		t.Fatalf("first project engaged = %q, want the provider project", first.Name)
	}
}

// A project created after startup is not part of the initial list and must
// still overtake the low-priority initial list.
func TestProjectEnqueuerNewProjectsOvertakeInitialList(t *testing.T) {
	q := priorityqueue.New[reconcile.Request]("test")
	defer q.ShutDown()
	e := &projectEnqueuer{isProvider: func(context.Context, string) bool { return false }}

	ctx := context.Background()
	e.Create(ctx, projectCreate("existing"), q)
	created := projectCreate("new-project")
	created.IsInInitialList = false
	e.Create(ctx, created, q)

	first, _ := q.Get()
	if first.Name != "new-project" {
		t.Fatalf("first project engaged = %q, want the newly created project", first.Name)
	}
}

func TestProviderProjectsListsServiceOwners(t *testing.T) {
	svc := &servicesv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{Name: "compute"}}
	svc.Spec.Owner.ProducerProjectRef.Name = "datum-cloud"
	failures := 1
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if failures > 0 {
					failures--
					return errors.New("unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	p := &providerProjects{reader: reader}

	ctx := context.Background()
	if p.has(ctx, "datum-cloud") {
		t.Fatal("has() = true although listing Services failed")
	}
	// The failed attempt is retried only after providerListRetryInterval.
	p.lastAttempt = p.lastAttempt.Add(-providerListRetryInterval)
	if !p.has(ctx, "datum-cloud") {
		t.Error("has(datum-cloud) = false, want true for a Service owner")
	}
	if p.has(ctx, "some-consumer") {
		t.Error("has(some-consumer) = true, want false")
	}
}
