// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

var (
	serviceEntitlementGVK = servicesv1alpha1.GroupVersion.WithKind("ServiceEntitlement")
	serviceConsumerGVK    = servicesv1alpha1.GroupVersion.WithKind("ServiceConsumer")
)

// finalizerFieldManager owns the finalizers this controller adds by apply.
const finalizerFieldManager = "services-finalizer"

// applyFinalizer adds a finalizer with server-side apply. Finalizers merge as
// a set and the apply carries no resourceVersion, so status writes from other
// controllers can't make it conflict. The apply is pinned to the object's UID
// so it can't recreate an object deleted since it was read. On success obj
// holds the server's latest copy.
//
// It is only applied while the finalizer is missing, so a later apply under
// the same manager can never omit, and so remove, it.
func applyFinalizer(ctx context.Context, c client.Client, obj client.Object, gvk schema.GroupVersionKind, finalizer string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(obj.GetName())
	u.SetNamespace(obj.GetNamespace())
	u.SetUID(obj.GetUID())
	u.SetFinalizers([]string{finalizer})
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(finalizerFieldManager)); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj)
}

// applySpec server-side applies spec as the spec fields manager owns and
// decodes the server's result into out. With force it takes over fields
// another manager last wrote; without, a different value another manager owns
// fails the apply with a conflict.
func applySpec(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, name string, spec map[string]any, manager string, force bool, out client.Object) error {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	opts := []client.ApplyOption{client.FieldOwner(manager)}
	if force {
		opts = append(opts, client.ForceOwnership)
	}
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), opts...); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}
