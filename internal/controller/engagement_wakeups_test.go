// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// expectWakeup engages name and asserts the wakeup delivers req.
func expectWakeup(t *testing.T, wakeups *engagementWakeups, name string, req mcreconcile.Request) {
	t.Helper()
	if err := wakeups.Engage(context.Background(), multicluster.ClusterName(name), nil); err != nil {
		t.Fatalf("Engage: %v", err)
	}
	select {
	case evt := <-wakeups.events:
		if evt.Object != req {
			t.Fatalf("wake-up request = %#v, want %#v", evt.Object, req)
		}
	case <-time.After(time.Second):
		t.Fatalf("engaging %q did not wake %v", name, req)
	}
}

// A provider project that is not engaged yet — routine right after the
// operator starts — must park the entitlement until that project engages,
// not push it onto exponential backoff that outlasts the engagement.
func TestServiceEntitlementReconcilerWaitsForProviderEngagement(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Finalizers = []string{serviceEntitlementFinalizer}
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	wakeups := newEngagementWakeups()
	r := &ServiceEntitlementReconciler{
		rootClient: newServiceRootClient(svc),
		Manager:    mgr,
		Scheme:     testScheme(),
		wakeups:    wakeups,
	}
	req := entitlementRequest(testConsumerProject, testServiceSlug)

	result, err := r.Reconcile(context.Background(), req)
	if err != nil || result != (ctrl.Result{}) {
		t.Fatalf("reconcile result=%+v err=%v, want engagement-driven wait", result, err)
	}

	providerClient := newFakeClient()
	mgr.add(testProviderProject, providerClient)
	expectWakeup(t, wakeups, testProviderProject, req)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after engagement: %v", err)
	}
	var sc servicesv1alpha1.ServiceConsumer
	key := types.NamespacedName{Name: serviceConsumerName(testServiceName, testConsumerProject)}
	if err := providerClient.Get(context.Background(), key, &sc); err != nil {
		t.Fatalf("provider consumer not created after engagement: %v", err)
	}
}

func TestServiceConsumerReconcilerWaitsForConsumerEngagement(t *testing.T) {
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	sc := newServiceConsumer(consumerName, &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	sc.Status.ServiceName = testServiceName
	mgr := newTestManager()
	mgr.add(testProviderProject, newFakeClient(sc))
	wakeups := newEngagementWakeups()
	r := &ServiceConsumerReconciler{Manager: mgr, Scheme: testScheme(), wakeups: wakeups}
	req := entitlementRequest(testProviderProject, consumerName)

	result, err := r.Reconcile(context.Background(), req)
	if err != nil || result != (ctrl.Result{}) {
		t.Fatalf("reconcile result=%+v err=%v, want engagement-driven wait", result, err)
	}

	expectWakeup(t, wakeups, testConsumerProject, req)
}

// engagesOnMiss engages a cluster the first time it is looked up and found
// missing, standing in for an engagement that lands between a reconciler's
// failed lookup and its wakeup registration.
type engagesOnMiss struct {
	*testManager
	name   string
	client client.Client
}

func (m *engagesOnMiss) GetCluster(ctx context.Context, name multicluster.ClusterName) (cluster.Cluster, error) {
	cl, err := m.testManager.GetCluster(ctx, name)
	if err != nil && string(name) == m.name {
		m.add(m.name, m.client)
	}
	return cl, err
}

func TestEngagementWakeupsRecheckAfterRegistering(t *testing.T) {
	mgr := &engagesOnMiss{testManager: newTestManager(), name: testProviderProject, client: newFakeClient()}
	wakeups := newEngagementWakeups()
	req := entitlementRequest(testConsumerProject, testServiceSlug)

	cl, ok, err := wakeups.cluster(context.Background(), mgr, multicluster.ClusterName(testProviderProject), req)
	if err != nil || !ok || cl == nil {
		t.Fatalf("cluster() = %v, %v, %v; want the cluster engaged during registration", cl, ok, err)
	}
	wakeups.mu.Lock()
	defer wakeups.mu.Unlock()
	if len(wakeups.waiting) != 0 {
		t.Fatalf("request left waiting after its cluster was found: %v", wakeups.waiting)
	}
}
