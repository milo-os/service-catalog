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

var serviceLog = logf.Log.WithName("service-webhook")

// dependenciesEqual reports whether two dependency lists reference the
// same Services in the same order. Used to skip cycle detection on
// updates that don't touch the dependency graph.
// Note: compares only ServiceRef.Name — sufficient because ServiceDependency
// has no other fields, but callers should be aware if the type grows.
func dependenciesEqual(a, b []servicesv1alpha1.ServiceDependency) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ServiceRef.Name != b[i].ServiceRef.Name {
			return false
		}
	}
	return true
}

// SetupServiceWebhookWithManager registers the Service webhook with
// the manager.
func SetupServiceWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceWebhook{
		client: mgr.GetClient(),
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr, &servicesv1alpha1.Service{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-services-miloapis-com-v1alpha1-service,mutating=true,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=services,verbs=create;update,versions=v1alpha1,name=mservice.kb.io,admissionReviewVersions=v1

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-service,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=services,verbs=create;update;delete,versions=v1alpha1,name=vservice.kb.io,admissionReviewVersions=v1

type serviceWebhook struct {
	client client.Client
	// reader is the uncached API reader; cycle detection lists every
	// Service and must not depend on informer warm-up.
	reader client.Reader
}

var _ admission.Defaulter[*servicesv1alpha1.Service] = &serviceWebhook{}
var _ admission.Validator[*servicesv1alpha1.Service] = &serviceWebhook{}

// Default implements webhook.CustomDefaulter. spec.phase defaults to
// Draft via the CRD's +kubebuilder:default marker, so admission-time
// defaulting is a no-op today; this hook is retained so future spec
// defaults can land here without a wiring change.
func (r *serviceWebhook) Default(ctx context.Context, svc *servicesv1alpha1.Service) error {
	serviceLog.Info("defaulting", "name", svc.GetName())
	return nil
}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceWebhook) ValidateCreate(ctx context.Context, svc *servicesv1alpha1.Service) (admission.Warnings, error) {
	serviceLog.Info("validating create",
		"name", svc.GetName(),
		"serviceName", svc.Spec.ServiceName,
	)

	errs := validation.ValidateServiceCreate(svc)
	errs = append(errs, validation.ValidateServiceDependencies(ctx, r.reader, svc)...)
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			svc.GetObjectKind().GroupVersionKind().GroupKind(),
			svc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceWebhook) ValidateUpdate(ctx context.Context, oldSvc, newSvc *servicesv1alpha1.Service) (admission.Warnings, error) {
	serviceLog.Info("validating update", "name", newSvc.GetName())

	errs := validation.ValidateServiceUpdate(oldSvc, newSvc)
	if !dependenciesEqual(oldSvc.Spec.Dependencies, newSvc.Spec.Dependencies) {
		errs = append(errs, validation.ValidateServiceDependencies(ctx, r.reader, newSvc)...)
	}
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newSvc.GetObjectKind().GroupVersionKind().GroupKind(),
			newSvc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today; when
// downstream references (MeterDefinition, MonitoredResourceType, etc.)
// hold the finalizer, this is the place to refuse deletion while any
// reference remains.
func (r *serviceWebhook) ValidateDelete(ctx context.Context, svc *servicesv1alpha1.Service) (admission.Warnings, error) {
	serviceLog.Info("validating delete", "name", svc.GetName())
	return nil, nil
}
