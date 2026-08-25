// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

var (
	// miloLocationGVK is the Location served by milo-os/locations, the home
	// this catalog reads locations from going forward.
	miloLocationGVK = schema.GroupVersionKind{
		Group:   "locations.miloapis.com",
		Version: "v1alpha1",
		Kind:    "Location",
	}

	// legacyLocationGVK is the network-services operator's Location. It is
	// still the only Location registered on control planes that have not taken
	// the locations service yet, including production today.
	legacyLocationGVK = schema.GroupVersionKind{
		Group:   "networking.datumapis.com",
		Version: "v1alpha",
		Kind:    "Location",
	}

	// locationGVKPreference orders the gate-2 read: the new group answers
	// wherever it is served, and the old group answers everywhere else. Both
	// are read as unstructured, which the controller-runtime client serves
	// straight from the API server rather than from an informer, so naming a
	// group whose CRD is absent costs a failed request instead of a cache that
	// never syncs.
	locationGVKPreference = []schema.GroupVersionKind{miloLocationGVK, legacyLocationGVK}
)

// getLocation reads the named Location from whichever group serves it,
// preferring locations.miloapis.com. It reports found=false when no group holds
// the object, and returns an error only for a read that failed for some other
// reason: a control plane without the newer CRD answers with a RESTMapper
// no-match, which is a miss rather than a failure.
func getLocation(ctx context.Context, c client.Reader, name string) (*unstructured.Unstructured, bool, error) {
	for _, gvk := range locationGVKPreference {
		loc := &unstructured.Unstructured{}
		loc.SetGroupVersionKind(gvk)
		err := c.Get(ctx, types.NamespacedName{Name: name}, loc)
		switch {
		case err == nil:
			return loc, true, nil
		case apierrors.IsNotFound(err), apimeta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
			continue
		default:
			return nil, false, err
		}
	}
	return nil, false, nil
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
