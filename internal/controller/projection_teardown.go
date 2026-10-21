// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ProjectionTeardown removes shared project projections when consumer-scoped
// engagement ends. Locations can be shared by several entitled services, so
// they deliberately have no service-name label and cannot use the consumer
// provider's service-scoped ManagedResources sweep.
type ProjectionTeardown struct{}

func (ProjectionTeardown) TeardownConsumer(
	ctx context.Context,
	consumerProject string,
	c client.Client,
	_ []string,
) error {
	location := &unstructured.Unstructured{}
	location.SetGroupVersionKind(LocationGVK)
	legacyBinding := &unstructured.Unstructured{}
	legacyBinding.SetGroupVersionKind(legacyLocationBindingGVK)
	objects := []client.Object{location, legacyBinding, &servicesv1alpha1.ServiceAvailability{}}
	for _, object := range objects {
		if err := c.DeleteAllOf(ctx, object, client.MatchingLabels{labelManagedBy: labelManagedByValue}); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete shared projections from consumer project %q: %w", consumerProject, err)
		}
	}
	return nil
}
