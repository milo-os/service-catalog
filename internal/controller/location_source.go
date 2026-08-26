// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// errLocationSourceUnavailable reports that the control plane does not serve
// the configured location source at all. It is a misconfiguration — an
// operator named a group that is not installed — and is kept distinct from a
// location that is merely absent, which is an ordinary closed gate.
var errLocationSourceUnavailable = errors.New("configured location source is not served by this control plane")

// getLocation reads the named Location from the configured source. It reports
// found=false when the source serves no such location, and wraps
// errLocationSourceUnavailable when the source itself is not served. There is
// no second attempt against another group: the source is chosen by
// configuration precisely so that nothing here decides it silently.
//
// The read is unstructured, which the controller-runtime client answers
// straight from the API server rather than through an informer. A group that
// is not installed therefore costs one failed request rather than a cache that
// never syncs and takes the manager down with it.
func getLocation(ctx context.Context, c client.Reader, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, bool, error) {
	loc := &unstructured.Unstructured{}
	loc.SetGroupVersionKind(gvk)
	err := c.Get(ctx, types.NamespacedName{Name: name}, loc)
	switch {
	case err == nil:
		return loc, true, nil
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case apimeta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
		return nil, false, fmt.Errorf("%w: %s: %w", errLocationSourceUnavailable, gvk.GroupVersion(), err)
	default:
		return nil, false, err
	}
}

// locationClass reads the class name off a Location of either group.
// locations.miloapis.com carries it as spec.locationClassRef.name;
// networking.datumapis.com carries it as the flat spec.locationClassName.
func locationClass(loc *unstructured.Unstructured) servicesv1alpha1.LocationClassName {
	if s, _, _ := unstructured.NestedString(loc.Object, "spec", "locationClassRef", "name"); s != "" {
		return servicesv1alpha1.LocationClassName(s)
	}
	s, _, _ := unstructured.NestedString(loc.Object, "spec", "locationClassName")
	return servicesv1alpha1.LocationClassName(s)
}
