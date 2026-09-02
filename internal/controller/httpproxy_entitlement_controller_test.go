// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const httpProxyTestProject = "alb-project"

func httpProxyScheme() *runtime.Scheme {
	s := testScheme()
	RegisterHTTPProxyScheme(s)
	return s
}

func newHTTPProxy(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(httpProxyGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func reconcileHTTPProxy(t *testing.T, cl client.Client, namespace, name string) {
	t.Helper()
	mgr := newTestManager()
	mgr.add(httpProxyTestProject, cl)
	r := &HTTPProxyEntitlementReconciler{Manager: mgr}
	_, err := r.Reconcile(context.Background(), mcreconcile.Request{
		ClusterName: httpProxyTestProject,
		Request: ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestHTTPProxyEntitlementReconciler_createsNetworkingEntitlement(t *testing.T) {
	t.Parallel()

	proxy := newHTTPProxy("default", "help-d9c5g5")
	cl := fake.NewClientBuilder().WithScheme(httpProxyScheme()).WithObjects(proxy).Build()
	reconcileHTTPProxy(t, cl, "default", "help-d9c5g5")

	var got servicesv1alpha1.ServiceEntitlement
	if err := cl.Get(context.Background(), types.NamespacedName{Name: networkingServiceObjectName}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Spec.ServiceRef.Name != networkingServiceObjectName {
		t.Errorf("spec.serviceRef.name = %q, want %q", got.Spec.ServiceRef.Name, networkingServiceObjectName)
	}
	if got.Labels[httpProxyEnrolledLabel] != "true" {
		t.Errorf("label %s = %q, want true", httpProxyEnrolledLabel, got.Labels[httpProxyEnrolledLabel])
	}
}

func TestHTTPProxyEntitlementReconciler_skipsWhenNetworkingAlreadyEntitled(t *testing.T) {
	t.Parallel()

	proxy := newHTTPProxy("default", "help-d9c5g5")
	existing := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "already-there"},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: networkingServiceCanonicalName},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(httpProxyScheme()).WithObjects(proxy, existing).Build()
	reconcileHTTPProxy(t, cl, "default", "help-d9c5g5")

	var created servicesv1alpha1.ServiceEntitlement
	err := cl.Get(context.Background(), types.NamespacedName{Name: networkingServiceObjectName}, &created)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no second entitlement, got err=%v", err)
	}
}

func TestHTTPProxyEntitlementReconciler_skipsMissingProxy(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().WithScheme(httpProxyScheme()).Build()
	reconcileHTTPProxy(t, cl, "default", "gone")

	var list servicesv1alpha1.ServiceEntitlementList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("got %d entitlements, want 0", len(list.Items))
	}
}

func TestHTTPProxyEntitlementReconciler_deletedProxyLeavesEntitlement(t *testing.T) {
	t.Parallel()

	existing := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: networkingServiceObjectName},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: networkingServiceObjectName},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(httpProxyScheme()).WithObjects(existing).Build()
	reconcileHTTPProxy(t, cl, "default", "gone")

	var got servicesv1alpha1.ServiceEntitlement
	if err := cl.Get(context.Background(), types.NamespacedName{Name: networkingServiceObjectName}, &got); err != nil {
		t.Fatalf("entitlement was removed after HTTPProxy delete: %v", err)
	}
}

func TestHTTPProxyEntitlementReconciler_terminatingProxyDoesNotEnroll(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	proxy := newHTTPProxy("default", "help-d9c5g5")
	proxy.SetDeletionTimestamp(&now)
	proxy.SetFinalizers([]string{"pending"})
	cl := fake.NewClientBuilder().WithScheme(httpProxyScheme()).WithObjects(proxy).Build()
	reconcileHTTPProxy(t, cl, "default", "help-d9c5g5")

	var list servicesv1alpha1.ServiceEntitlementList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("got %d entitlements, want 0", len(list.Items))
	}
}

func TestHTTPProxyEntitlementReconciler_unservedHTTPProxyIsNoop(t *testing.T) {
	t.Parallel()

	base := fake.NewClientBuilder().WithScheme(httpProxyScheme()).Build()
	cl := interceptor.NewClient(base, noMatchOn(httpProxyGVK))
	reconcileHTTPProxy(t, cl, "default", "help-d9c5g5")

	var list servicesv1alpha1.ServiceEntitlementList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("got %d entitlements, want 0", len(list.Items))
	}
}

func TestIsNetworkingEntitlement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		se   *servicesv1alpha1.ServiceEntitlement
		want bool
	}{
		{
			name: "object name",
			se: &servicesv1alpha1.ServiceEntitlement{
				Spec: servicesv1alpha1.ServiceEntitlementSpec{
					ServiceRef: servicesv1alpha1.ServiceRef{Name: networkingServiceObjectName},
				},
			},
			want: true,
		},
		{
			name: "canonical spec name",
			se: &servicesv1alpha1.ServiceEntitlement{
				Spec: servicesv1alpha1.ServiceEntitlementSpec{
					ServiceRef: servicesv1alpha1.ServiceRef{Name: networkingServiceCanonicalName},
				},
			},
			want: true,
		},
		{
			name: "stamped canonical status",
			se: &servicesv1alpha1.ServiceEntitlement{
				Spec: servicesv1alpha1.ServiceEntitlementSpec{
					ServiceRef: servicesv1alpha1.ServiceRef{Name: "other-name"},
				},
				Status: servicesv1alpha1.ServiceEntitlementStatus{
					ServiceName: networkingServiceCanonicalName,
				},
			},
			want: true,
		},
		{
			name: "other service",
			se: &servicesv1alpha1.ServiceEntitlement{
				Spec: servicesv1alpha1.ServiceEntitlementSpec{
					ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"},
				},
				Status: servicesv1alpha1.ServiceEntitlementStatus{ServiceName: "compute.datumapis.com"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isNetworkingEntitlement(tt.se); got != tt.want {
				t.Errorf("isNetworkingEntitlement() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureNetworkingEntitlement_alreadyExistsRace(t *testing.T) {
	t.Parallel()

	existing := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: networkingServiceObjectName},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: networkingServiceObjectName},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(httpProxyScheme()).WithObjects(existing).Build()
	if err := ensureNetworkingEntitlement(context.Background(), cl); err != nil {
		t.Fatalf("ensureNetworkingEntitlement: %v", err)
	}

	var list servicesv1alpha1.ServiceEntitlementList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("got %d entitlements, want 1", len(list.Items))
	}
}

func TestEnsureNetworkingEntitlement_createAlreadyExists(t *testing.T) {
	t.Parallel()

	base := fake.NewClientBuilder().WithScheme(httpProxyScheme()).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(
				schema.GroupResource{Group: "services.miloapis.com", Resource: "serviceentitlements"},
				obj.GetName(),
			)
		},
	})
	if err := ensureNetworkingEntitlement(context.Background(), cl); err != nil {
		t.Fatalf("ensureNetworkingEntitlement: %v", err)
	}
}
