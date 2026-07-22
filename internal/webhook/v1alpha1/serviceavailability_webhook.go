// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/validation"
)

var serviceAvailabilityLog = logf.Log.WithName("serviceavailability-webhook")

// SetupServiceAvailabilityWebhookWithManager registers the
// ServiceAvailability validating webhook with the manager.
func SetupServiceAvailabilityWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceAvailabilityWebhook{
		// Use the API reader (uncached) so Service lookups during
		// admission don't depend on informer warm-up.
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr, &servicesv1alpha1.ServiceAvailability{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceavailability,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceavailabilities,verbs=create;update,versions=v1alpha1,name=vserviceavailability.kb.io,admissionReviewVersions=v1

type serviceAvailabilityWebhook struct {
	reader client.Reader
}

var _ admission.Validator[*servicesv1alpha1.ServiceAvailability] = &serviceAvailabilityWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceAvailabilityWebhook) ValidateCreate(ctx context.Context, sa *servicesv1alpha1.ServiceAvailability) (admission.Warnings, error) {
	serviceAvailabilityLog.Info("validating create",
		"name", sa.GetName(),
		"serviceRef", sa.Spec.ServiceRef.Name,
		"locationRef", sa.Spec.LocationRef.Name,
	)

	if errs := validation.ValidateServiceAvailabilityCreate(ctx, r.reader, sa); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			sa.GetObjectKind().GroupVersionKind().GroupKind(),
			sa.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceAvailabilityWebhook) ValidateUpdate(ctx context.Context, oldSA, newSA *servicesv1alpha1.ServiceAvailability) (admission.Warnings, error) {
	serviceAvailabilityLog.Info("validating update", "name", newSA.GetName())

	if errs := validation.ValidateServiceAvailabilityUpdate(ctx, r.reader, oldSA, newSA); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newSA.GetObjectKind().GroupVersionKind().GroupKind(),
			newSA.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. ServiceAvailability
// has no delete-time invariants; deletion is always permitted.
func (r *serviceAvailabilityWebhook) ValidateDelete(ctx context.Context, _ *servicesv1alpha1.ServiceAvailability) (admission.Warnings, error) {
	return nil, nil
}
