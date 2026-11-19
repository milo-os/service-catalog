// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func consumerOfProject(project string) *servicesv1alpha1.ServiceConsumer {
	sc := newServiceConsumer(serviceConsumerName(testServiceName, project), nil)
	sc.Spec.ConsumerProjectRef.Name = project
	sc.Status.Phase = servicesv1alpha1.ConsumerPhaseActive
	return sc
}

func liveProject(name string) *resourcemanagerv1alpha1.Project {
	return &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func newRootClient(projects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(projects...).Build()
}

// goneProjectReconciler returns a reconciler whose provider project holds the
// given consumers. cache and live stand for the root informer and the root API.
func goneProjectReconciler(cache, live client.Reader, pacer *deletePacer, consumers ...client.Object) (*ServiceConsumerReconciler, client.Client) {
	providerClient := newFakeClient(consumers...)
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	return &ServiceConsumerReconciler{
		Manager: mgr, Scheme: testScheme(), rootClient: cache, rootAPIReader: live, goneDeletes: pacer,
	}, providerClient
}

func consumerExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var sc servicesv1alpha1.ServiceConsumer
	err := c.Get(context.Background(), types.NamespacedName{Name: name}, &sc)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get consumer %q: %v", name, err)
	}
	return sc.DeletionTimestamp.IsZero()
}

func TestServiceConsumerReconciler_ProjectMissingFromCacheIsConfirmedLive(t *testing.T) {
	sc := consumerOfProject("new-project")
	r, providerClient := goneProjectReconciler(newRootClient(), newRootClient(liveProject("new-project")), nil, sc)

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, sc.Name)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !consumerExists(t, providerClient, sc.Name) {
		t.Fatal("a consumer whose Project the cache had not seen yet was deleted")
	}
}

func TestServiceConsumerReconciler_KeepsConsumerOfDeletingProject(t *testing.T) {
	deleting := liveProject("leaving")
	deleting.Finalizers = []string{"resourcemanager.miloapis.com/project-controller"}
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	root := newRootClient(deleting)
	sc := consumerOfProject("leaving")
	r, providerClient := goneProjectReconciler(root, root, nil, sc)

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, sc.Name)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !consumerExists(t, providerClient, sc.Name) {
		t.Fatal("the consumer of a deleting Project was deleted; the entitlement path ends it")
	}
}

func TestServiceConsumerReconciler_RootReadErrorKeepsConsumer(t *testing.T) {
	forbidden := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "resourcemanager.miloapis.com", Resource: "projects"}, "p", nil)
	}}
	root := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(forbidden).Build()
	sc := consumerOfProject("unreadable")
	r, providerClient := goneProjectReconciler(root, root, nil, sc)

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, sc.Name)); err == nil {
		t.Fatal("expected the root read error to be returned")
	}
	if !consumerExists(t, providerClient, sc.Name) {
		t.Fatal("a consumer was deleted although its Project could not be read")
	}
}

func TestServiceConsumerReconciler_DeletesPendingConsumerOfDeletedProject(t *testing.T) {
	sc := consumerOfProject("gone")
	sc.Status.Phase = servicesv1alpha1.ConsumerPhasePendingApproval
	r, providerClient := goneProjectReconciler(newRootClient(), newRootClient(), nil, sc)

	if _, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, sc.Name)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if consumerExists(t, providerClient, sc.Name) {
		t.Fatal("a pending consumer of a deleted Project was kept")
	}
}

// The first gone consumer takes the bucket's one token; the rest book slots.
func TestServiceConsumerReconciler_ReleasesSlotWhenNoLongerNeeded(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	pacer := newDeletePacer(3*time.Second, func() time.Time { return now })
	first, removed, recreated := consumerOfProject("gone-a"), consumerOfProject("gone-b"), consumerOfProject("gone-c")
	root := newRootClient()
	r, providerClient := goneProjectReconciler(root, root, pacer, first, removed, recreated)
	ctx := context.Background()
	for _, sc := range []*servicesv1alpha1.ServiceConsumer{first, removed, recreated} {
		if _, err := r.Reconcile(ctx, entitlementRequest(testProviderProject, sc.Name)); err != nil {
			t.Fatalf("reconcile %s: %v", sc.Name, err)
		}
	}
	if len(pacer.slots) != 2 {
		t.Fatalf("expected two booked slots, got %d", len(pacer.slots))
	}

	// One consumer is deleted by someone else; another's Project is created again.
	if err := providerClient.Delete(ctx, removed); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := root.Create(ctx, liveProject("gone-c")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now = now.Add(time.Minute)
	for _, sc := range []*servicesv1alpha1.ServiceConsumer{removed, recreated} {
		if _, err := r.Reconcile(ctx, entitlementRequest(testProviderProject, sc.Name)); err != nil {
			t.Fatalf("reconcile %s: %v", sc.Name, err)
		}
	}
	if len(pacer.slots) != 0 {
		t.Fatalf("expected no slots left, got %v", pacer.slots)
	}
	if !consumerExists(t, providerClient, recreated.Name) {
		t.Fatal("the consumer of a Project created again under its name was deleted")
	}
}

// TestServiceConsumerReconciler_PacesDeletesOfDeletedProjects drives the
// reconciler the way its queue would: each request comes back when its
// RequeueAfter is due. Every gone consumer must be deleted, one per interval,
// without a reconcile ever blocking, while a live consumer is never held.
func TestServiceConsumerReconciler_PacesDeletesOfDeletedProjects(t *testing.T) {
	const gone = 50
	interval := 3 * time.Second
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	pacer := newDeletePacer(interval, func() time.Time { return now })

	due := map[string]time.Time{}
	var consumers []client.Object
	for i := 0; i < gone; i++ {
		sc := consumerOfProject(fmt.Sprintf("gone-%02d", i))
		consumers = append(consumers, sc)
		due[sc.Name] = now
	}
	live := consumerOfProject("live")
	consumers = append(consumers, live)
	due[live.Name] = now
	root := newRootClient(liveProject("live"))
	r, providerClient := goneProjectReconciler(root, root, pacer, consumers...)
	start := now

	finished := driveQueue(t, r, &now, due)

	if at, ok := finished[live.Name]; !ok || !at.Equal(start) || !consumerExists(t, providerClient, live.Name) {
		t.Fatal("the live consumer was held or deleted")
	}
	delete(finished, live.Name)
	assertPacedDeletes(t, providerClient, finished, gone, interval)
	if len(pacer.slots) != 0 {
		t.Fatalf("expected no slots left after the drain, got %d", len(pacer.slots))
	}
}

// driveQueue reconciles each request when it is due, as the workqueue would,
// and returns when each request finished without asking to be requeued. A
// reconcile must never block: a waiting request is requeued, not held.
func driveQueue(t *testing.T, r *ServiceConsumerReconciler, now *time.Time, due map[string]time.Time) map[string]time.Time {
	t.Helper()
	finished := map[string]time.Time{}
	for steps, limit := 0, 10*len(due); len(due) > 0 && steps < limit; steps++ {
		name := earliestDue(due)
		*now = due[name]
		delete(due, name)
		started := time.Now()
		res, err := r.Reconcile(context.Background(), entitlementRequest(testProviderProject, name))
		if err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
		if took := time.Since(started); took > time.Second {
			t.Fatalf("reconcile %s blocked for %s; it must requeue instead", name, took)
		}
		if res.RequeueAfter > 0 {
			due[name] = now.Add(res.RequeueAfter)
			continue
		}
		finished[name] = *now
	}
	return finished
}

// assertPacedDeletes checks that every consumer in finished is gone and that
// the deletes came at least interval apart.
func assertPacedDeletes(t *testing.T, c client.Client, finished map[string]time.Time, want int, interval time.Duration) {
	t.Helper()
	deletedAt := make([]time.Time, 0, len(finished))
	for name, at := range finished {
		if consumerExists(t, c, name) {
			t.Fatalf("consumer %s of a deleted project was kept", name)
		}
		deletedAt = append(deletedAt, at)
	}
	if len(deletedAt) != want {
		t.Fatalf("deleted %d of %d consumers of deleted projects", len(deletedAt), want)
	}
	sort.Slice(deletedAt, func(i, j int) bool { return deletedAt[i].Before(deletedAt[j]) })
	for i := 1; i < len(deletedAt); i++ {
		if gap := deletedAt[i].Sub(deletedAt[i-1]); gap < interval {
			t.Fatalf("deletes %d and %d are %s apart, want at least %s", i-1, i, gap, interval)
		}
	}
}

// earliestDue returns the request due first, by name on a tie.
func earliestDue(due map[string]time.Time) string {
	names := make([]string, 0, len(due))
	for name := range due {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if !due[names[i]].Equal(due[names[j]]) {
			return due[names[i]].Before(due[names[j]])
		}
		return names[i] < names[j]
	})
	return names[0]
}
