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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func TestAddFinalizerLiveRecoversFromStaleCopy(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(availabilityScheme()).
		WithObjects(&servicesv1alpha1.ServiceEntitlement{ObjectMeta: metav1.ObjectMeta{Name: "compute"}}).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).Build()
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

	if err := addFinalizerLive(ctx, c, c, &stale, serviceEntitlementFinalizer); err != nil {
		t.Fatalf("addFinalizerLive: %v", err)
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

func TestUpdateLiveAppliesMutationToExistingObject(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(availabilityScheme()).
		WithObjects(&servicesv1alpha1.ServiceConsumer{
			ObjectMeta: metav1.ObjectMeta{Name: "sc-1"},
			Spec:       servicesv1alpha1.ServiceConsumerSpec{RequestMessage: "old"},
		}).Build()
	ctx := context.Background()

	consumer := &servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: "sc-1"}}
	mutate := func() error { consumer.Spec.RequestMessage = "new"; return nil }
	op, err := updateLive(ctx, c, c, consumer, mutate)
	if err != nil || op != controllerutil.OperationResultUpdated {
		t.Fatalf("updateLive = %v, %v; want updated", op, err)
	}
	op, err = updateLive(ctx, c, c, &servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: "sc-1"}}, func() error { return nil })
	if err != nil || op != controllerutil.OperationResultNone {
		t.Fatalf("second updateLive = %v, %v; want no change", op, err)
	}
	var got servicesv1alpha1.ServiceConsumer
	if err := c.Get(ctx, client.ObjectKey{Name: "sc-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.RequestMessage != "new" {
		t.Errorf("requestMessage = %q, want new", got.Spec.RequestMessage)
	}
}
