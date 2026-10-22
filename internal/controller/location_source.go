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

var LocationGVK = schema.GroupVersionKind{
	Group: "locations.miloapis.com", Version: "v1alpha1", Kind: "Location",
}

// errLocationAPIUnavailable reports that the control plane does not serve
// the Location API at all. It is kept distinct from a
// location that is merely absent, which is an ordinary closed gate.
var errLocationAPIUnavailable = errors.New("location API is not served by this control plane")

// getLocation reads a named locations.miloapis.com Location. It reports
// found=false when no such location exists and wraps errLocationAPIUnavailable
// when the API itself is not served.
//
// The selected unstructured GVK is registered before manager construction, so
// the injected manager reader answers this from its shared informer cache. A
// cache miss after synchronization is a legitimate absent Location; source
// discovery and initial synchronization failures prevent the controller from
// starting and therefore cannot be mistaken for an empty desired set.
func getLocation(ctx context.Context, c client.Reader, name string) (*unstructured.Unstructured, bool, error) {
	loc := &unstructured.Unstructured{}
	loc.SetGroupVersionKind(LocationGVK)
	err := c.Get(ctx, types.NamespacedName{Name: name}, loc)
	switch {
	case err == nil:
		return loc, true, nil
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case apimeta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
		return nil, false, fmt.Errorf("%w: %s: %w", errLocationAPIUnavailable, LocationGVK.GroupVersion(), err)
	default:
		return nil, false, err
	}
}

// locationClass reads the referenced LocationClass name.
func locationClass(loc *unstructured.Unstructured) servicesv1alpha1.LocationClassName {
	s, _, _ := unstructured.NestedString(loc.Object, "spec", "locationClassRef", "name")
	return servicesv1alpha1.LocationClassName(s)
}
