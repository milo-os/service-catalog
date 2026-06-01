// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	saName        = "compute--us-central1-a"
	saServiceName = "compute.miloapis.com"
	saLocName     = "us-central1-a"
	saLocNS       = "platform"
)

// availabilityScheme registers the services types plus the foreign Location
// GVK as unstructured so the fake client can serve gate-2 reads.
func availabilityScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(locationGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(locationGVK.GroupVersion().WithKind("LocationList"), &unstructured.UnstructuredList{})
	return s
}

func newAvailabilityClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(availabilityScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&servicesv1alpha1.ServiceAvailability{}).
		Build()
}

func newAvailability() *servicesv1alpha1.ServiceAvailability {
	return &servicesv1alpha1.ServiceAvailability{
		ObjectMeta: metav1.ObjectMeta{Name: saName},
		Spec: servicesv1alpha1.ServiceAvailabilitySpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: saServiceName},
			LocationRef: servicesv1alpha1.LocationRef{Name: saLocName, Namespace: saLocNS},
		},
	}
}

func newServiceInPhase(name string, phase servicesv1alpha1.Phase) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: name,
			Phase:       phase,
		},
	}
}

func newLocation(name, namespace string, ready bool) *unstructured.Unstructured {
	status := string(metav1.ConditionFalse)
	if ready {
		status = string(metav1.ConditionTrue)
	}
	loc := &unstructured.Unstructured{}
	loc.SetGroupVersionKind(locationGVK)
	loc.SetName(name)
	loc.SetNamespace(namespace)
	_ = unstructured.SetNestedSlice(loc.Object, []any{
		map[string]any{"type": "Ready", "status": status},
	}, "status", "conditions")
	return loc
}

// reconcileAvailability runs one reconcile pass and returns the refreshed
// object.
func reconcileAvailability(t *testing.T, c client.Client) *servicesv1alpha1.ServiceAvailability {
	t.Helper()
	r := &ServiceAvailabilityReconciler{client: c, Scheme: availabilityScheme()}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: saName}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got servicesv1alpha1.ServiceAvailability
	if err := c.Get(context.Background(), types.NamespacedName{Name: saName}, &got); err != nil {
		t.Fatalf("get serviceavailability: %v", err)
	}
	return &got
}

func assertAvailable(t *testing.T, sa *servicesv1alpha1.ServiceAvailability, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	cond := apimeta.FindStatusCondition(sa.Status.Conditions, ConditionTypeAvailable)
	if cond == nil {
		t.Fatalf("Available condition not set")
	}
	if cond.Status != wantStatus {
		t.Errorf("Available status = %q, want %q (reason %q)", cond.Status, wantStatus, cond.Reason)
	}
	if cond.Reason != wantReason {
		t.Errorf("Available reason = %q, want %q", cond.Reason, wantReason)
	}
	if sa.Status.ObservedGeneration != sa.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", sa.Status.ObservedGeneration, sa.Generation)
	}
}

func TestServiceAvailabilityReconciler_Available(t *testing.T) {
	c := newAvailabilityClient(
		newAvailability(),
		newServiceInPhase(saServiceName, servicesv1alpha1.PhasePublished),
		newLocation(saLocName, saLocNS, true),
	)
	got := reconcileAvailability(t, c)
	assertAvailable(t, got, metav1.ConditionTrue, reasonServiceOperational)
}

func TestServiceAvailabilityReconciler_ServiceNotPublished(t *testing.T) {
	c := newAvailabilityClient(
		newAvailability(),
		newServiceInPhase(saServiceName, servicesv1alpha1.PhaseDraft),
		newLocation(saLocName, saLocNS, true),
	)
	got := reconcileAvailability(t, c)
	assertAvailable(t, got, metav1.ConditionFalse, reasonServiceNotPublished)
}

func TestServiceAvailabilityReconciler_ServiceNotFound(t *testing.T) {
	c := newAvailabilityClient(
		newAvailability(),
		newLocation(saLocName, saLocNS, true),
	)
	got := reconcileAvailability(t, c)
	assertAvailable(t, got, metav1.ConditionFalse, reasonServiceNotPublished)
}

func TestServiceAvailabilityReconciler_LocationNotReady(t *testing.T) {
	c := newAvailabilityClient(
		newAvailability(),
		newServiceInPhase(saServiceName, servicesv1alpha1.PhasePublished),
		newLocation(saLocName, saLocNS, false),
	)
	got := reconcileAvailability(t, c)
	assertAvailable(t, got, metav1.ConditionFalse, reasonLocationNotReady)
}

func TestServiceAvailabilityReconciler_LocationNotFound(t *testing.T) {
	c := newAvailabilityClient(
		newAvailability(),
		newServiceInPhase(saServiceName, servicesv1alpha1.PhasePublished),
	)
	got := reconcileAvailability(t, c)
	assertAvailable(t, got, metav1.ConditionFalse, reasonLocationNotFound)
}

// TestServiceAvailabilityReconciler_Idempotent locks in patch-only-when-changed:
// a second reconcile over an already-settled object must not write status (no
// resourceVersion bump, no condition timestamp churn).
func TestServiceAvailabilityReconciler_Idempotent(t *testing.T) {
	c := newAvailabilityClient(
		newAvailability(),
		newServiceInPhase(saServiceName, servicesv1alpha1.PhasePublished),
		newLocation(saLocName, saLocNS, true),
	)
	first := reconcileAvailability(t, c)
	assertAvailable(t, first, metav1.ConditionTrue, reasonServiceOperational)

	second := reconcileAvailability(t, c)
	if second.ResourceVersion != first.ResourceVersion {
		t.Errorf("second reconcile wrote status (resourceVersion %s -> %s); expected a no-op",
			first.ResourceVersion, second.ResourceVersion)
	}
}

// TestServiceAvailabilityReconciler_TransientErrorRequeues locks in the
// requeue-don't-flip behavior: a non-NotFound read error on the referenced
// Service must surface as a reconcile error (requeue) and must not flip the
// Available condition.
func TestServiceAvailabilityReconciler_TransientErrorRequeues(t *testing.T) {
	boom := errors.New("apiserver temporarily unavailable")
	base := fake.NewClientBuilder().
		WithScheme(availabilityScheme()).
		WithObjects(
			newAvailability(),
			newServiceInPhase(saServiceName, servicesv1alpha1.PhasePublished),
			newLocation(saLocName, saLocNS, true),
		).
		WithStatusSubresource(&servicesv1alpha1.ServiceAvailability{}).
		Build()

	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*servicesv1alpha1.Service); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})

	r := &ServiceAvailabilityReconciler{client: c, Scheme: availabilityScheme()}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: saName}}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatalf("expected a requeue error from the transient Service read failure, got nil")
	}

	var got servicesv1alpha1.ServiceAvailability
	if err := base.Get(context.Background(), types.NamespacedName{Name: saName}, &got); err != nil {
		t.Fatalf("get serviceavailability: %v", err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeAvailable); cond != nil {
		t.Errorf("Available condition was written despite a transient error: %+v", cond)
	}
}
