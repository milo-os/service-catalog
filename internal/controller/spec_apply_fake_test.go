// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Apply emulates the API server for the partial applies applyFinalizer and
// applySpec send. The fake client decodes an apply into the typed object, so
// fields the apply leaves out come back as zero values it then tries to own,
// and a finalizer-only apply conflicts on spec. Here the applied fields merge
// into the live object: maps by key, finalizers as a set, and a different
// spec.approval is refused as a conflict, as an unforced apply would be.
// spec_apply_envtest_test.go checks the same writes against a real API server.
func (c *ssaClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	applied := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &applied.Object); err != nil {
		return err
	}
	gvk := applied.GroupVersionKind()
	if gvk != serviceEntitlementGVK && gvk != serviceConsumerGVK {
		return c.Client.Apply(ctx, obj, opts...)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gvk)
	err = c.Get(ctx, client.ObjectKey{Name: applied.GetName()}, live)
	switch {
	case apierrors.IsNotFound(err):
		if applied.GetUID() != "" {
			return err
		}
		live = applied.DeepCopy()
		if err := c.Create(ctx, live); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if uid := applied.GetUID(); uid != "" && uid != live.GetUID() {
			return apierrors.NewConflict(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, live.GetName(), nil)
		}
		if want, ok, _ := unstructured.NestedMap(applied.Object, "spec", "approval"); ok {
			if have, found, _ := unstructured.NestedMap(live.Object, "spec", "approval"); found && !equality.Semantic.DeepEqual(have, want) {
				return apierrors.NewConflict(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, live.GetName(), nil)
			}
		}
		finalizers := live.GetFinalizers()
		for _, f := range applied.GetFinalizers() {
			if !slices.Contains(finalizers, f) {
				finalizers = append(finalizers, f)
			}
		}
		if spec, ok := applied.Object["spec"].(map[string]any); ok {
			merged, _ := live.Object["spec"].(map[string]any)
			if merged == nil {
				merged = map[string]any{}
			}
			for k, v := range spec {
				merged[k] = v
			}
			live.Object["spec"] = merged
		}
		live.SetFinalizers(finalizers)
		if err := c.Update(ctx, live); err != nil {
			return err
		}
	}

	out, err := json.Marshal(live.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, obj)
}
