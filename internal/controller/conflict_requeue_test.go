// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
