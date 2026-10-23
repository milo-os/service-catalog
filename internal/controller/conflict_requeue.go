// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// conflictRequeueDelay is how long a pass that lost an optimistic-lock race
// waits before running again: long enough for the informer cache to observe
// the newer object, short enough to stay inside interactive setup latency.
const conflictRequeueDelay = 250 * time.Millisecond

// conflictRequeueReconciler runs a pass again shortly when it failed only
// because another writer updated the object first.
//
// ServiceEntitlement status is server-side applied and never conflicts, but
// finalizer updates, ServiceConsumer and activation request status, and the
// one-time status ownership migration still use optimistic locking, and those
// race routinely while setup is in flight. Returned as errors, they feed the
// workqueue's per-item exponential backoff, and a busy project quickly waits
// seconds between passes. A conflict carries no failure to back off from: the
// newer object already exists and its update event re-enqueues the item too.
// The fixed short delay keeps convergence prompt without relying on that event.
type conflictRequeueReconciler[request comparable] struct {
	wrapped reconcile.TypedReconciler[request]
}

func requeueOnConflict[request comparable](r reconcile.TypedReconciler[request]) reconcile.TypedReconciler[request] {
	return &conflictRequeueReconciler[request]{wrapped: r}
}

func (r *conflictRequeueReconciler[request]) Reconcile(ctx context.Context, req request) (reconcile.Result, error) {
	result, err := r.wrapped.Reconcile(ctx, req)
	if err != nil && apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("lost an update race; running again shortly", "reason", err.Error())
		return reconcile.Result{RequeueAfter: conflictRequeueDelay}, nil
	}
	return result, err
}

func (r *conflictRequeueReconciler[request]) String() string {
	return fmt.Sprintf("%v", r.wrapped)
}

// addFinalizerLive adds a finalizer, retrying a lost update race against a
// live read instead of the cache. A new object's status is written by several
// controllers at once, so the cached copy is routinely a version behind and
// each conflict would otherwise cost a requeue.
func addFinalizerLive(ctx context.Context, c client.Client, reader client.Reader, obj client.Object, finalizer string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// A deleting object takes no new finalizers; the caller sees the
		// deletion timestamp and stops.
		if !obj.GetDeletionTimestamp().IsZero() || !controllerutil.AddFinalizer(obj, finalizer) {
			return nil
		}
		err := c.Update(ctx, obj)
		if apierrors.IsConflict(err) {
			if getErr := reader.Get(ctx, client.ObjectKeyFromObject(obj), obj); getErr != nil {
				return getErr
			}
		}
		return err
	})
}

// updateLive applies mutate to the live object and updates it if anything
// changed. It recovers a create that lost to another writer the cache hadn't
// seen yet. obj must carry only its key.
func updateLive(ctx context.Context, c client.Client, reader client.Reader, obj client.Object, mutate controllerutil.MutateFn) (controllerutil.OperationResult, error) {
	if err := reader.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	before := obj.DeepCopyObject()
	if err := mutate(); err != nil {
		return controllerutil.OperationResultNone, err
	}
	if equality.Semantic.DeepEqual(before, obj) {
		return controllerutil.OperationResultNone, nil
	}
	if err := c.Update(ctx, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	return controllerutil.OperationResultUpdated, nil
}
