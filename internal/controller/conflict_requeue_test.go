// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestRequeueOnConflictRetriesQuietly(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Group: "services.miloapis.com", Resource: "serviceentitlements"}, "compute", errors.New("the object has been modified"))
	other := errors.New("provider unavailable")
	for name, tc := range map[string]struct {
		err         error
		wantErr     error
		wantRequeue bool
	}{
		"conflict":         {err: conflict, wantRequeue: true},
		"wrapped conflict": {err: fmt.Errorf("patch setup status: %w", conflict), wantRequeue: true},
		"other error":      {err: other, wantErr: other},
		"success":          {},
	} {
		t.Run(name, func(t *testing.T) {
			r := requeueOnConflict[reconcile.Request](reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
				return reconcile.Result{}, tc.err
			}))
			result, err := r.Reconcile(context.Background(), reconcile.Request{})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got := result.RequeueAfter == conflictRequeueDelay; got != tc.wantRequeue {
				t.Errorf("requeueAfter = %s, want short requeue %v", result.RequeueAfter, tc.wantRequeue)
			}
		})
	}
}

func TestApplyFinalizerIgnoresUnrelatedWrites(t *testing.T) {
	c := newFakeClient(&servicesv1alpha1.ServiceEntitlement{ObjectMeta: metav1.ObjectMeta{Name: "compute"}})
	ctx := context.Background()

	var stale servicesv1alpha1.ServiceEntitlement
	if err := c.Get(ctx, client.ObjectKey{Name: "compute"}, &stale); err != nil {
		t.Fatal(err)
	}
	// Another controller writes status, so the copy in hand is a version behind.
	var other servicesv1alpha1.ServiceEntitlement
	if err := c.Get(ctx, client.ObjectKey{Name: "compute"}, &other); err != nil {
		t.Fatal(err)
	}
	other.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
	if err := c.Status().Update(ctx, &other); err != nil {
		t.Fatal(err)
	}

	if err := applyFinalizer(ctx, c, &stale, serviceEntitlementGVK, serviceEntitlementFinalizer); err != nil {
		t.Fatalf("applyFinalizer: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&stale, serviceEntitlementFinalizer) || stale.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("object after apply = finalizers %v, phase %q; want the server's latest copy", stale.Finalizers, stale.Status.Phase)
	}
	var got servicesv1alpha1.ServiceEntitlement
	if err := c.Get(ctx, client.ObjectKey{Name: "compute"}, &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, serviceEntitlementFinalizer) {
		t.Fatalf("finalizers = %v, want %q", got.Finalizers, serviceEntitlementFinalizer)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("status phase = %q, want the other writer's update kept", got.Status.Phase)
	}
}

func TestApplyFinalizerKeepsOtherFinalizers(t *testing.T) {
	c := newFakeClient(&servicesv1alpha1.ServiceEntitlement{ObjectMeta: metav1.ObjectMeta{Name: "compute", Finalizers: []string{"example.com/other"}}})
	ctx := context.Background()
	var e servicesv1alpha1.ServiceEntitlement
	if err := c.Get(ctx, client.ObjectKey{Name: "compute"}, &e); err != nil {
		t.Fatal(err)
	}
	if err := applyFinalizer(ctx, c, &e, serviceEntitlementGVK, serviceEntitlementFinalizer); err != nil {
		t.Fatalf("applyFinalizer: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&e, "example.com/other") || !controllerutil.ContainsFinalizer(&e, serviceEntitlementFinalizer) {
		t.Errorf("finalizers = %v, want both", e.Finalizers)
	}
}
