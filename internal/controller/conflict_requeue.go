// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// Several controllers write the same ServiceEntitlement status, so conflicts
// are routine while setup is in flight. Returned as errors, they feed the
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
