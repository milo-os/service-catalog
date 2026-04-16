// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
	"go.miloapis.com/services/internal/validation"
)

var serviceLog = logf.Log.WithName("service-webhook")

// SetupServiceWebhookWithManager registers the Service webhook with
// the manager.
func SetupServiceWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceWebhook{
		client: mgr.GetClient(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.Service{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-services-miloapis-com-v1alpha1-service,mutating=true,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=services,verbs=create;update,versions=v1alpha1,name=mservice.kb.io,admissionReviewVersions=v1

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-service,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=services,verbs=create;update;delete,versions=v1alpha1,name=vservice.kb.io,admissionReviewVersions=v1

type serviceWebhook struct {
	client client.Client
}

var _ admission.CustomDefaulter = &serviceWebhook{}
var _ admission.CustomValidator = &serviceWebhook{}

// Default implements webhook.CustomDefaulter. spec.phase defaults to
// Draft via the CRD's +kubebuilder:default marker, so admission-time
// defaulting is a no-op today; this hook is retained so future spec
// defaults can land here without a wiring change.
func (r *serviceWebhook) Default(ctx context.Context, obj runtime.Object) error {
	svc, ok := obj.(*servicesv1alpha1.Service)
	if !ok {
		return fmt.Errorf("unexpected type %T", obj)
	}
	serviceLog.Info("defaulting", "name", svc.GetName())
	return nil
}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	svc, ok := obj.(*servicesv1alpha1.Service)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceLog.Info("validating create",
		"name", svc.GetName(),
		"serviceName", svc.Spec.ServiceName,
	)

	if errs := validation.ValidateServiceCreate(svc); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			svc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSvc, ok := oldObj.(*servicesv1alpha1.Service)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newSvc, ok := newObj.(*servicesv1alpha1.Service)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}
	serviceLog.Info("validating update", "name", newSvc.GetName())

	if errs := validation.ValidateServiceUpdate(oldSvc, newSvc); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
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
func (r *serviceWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	svc, ok := obj.(*servicesv1alpha1.Service)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceLog.Info("validating delete", "name", svc.GetName())
	return nil, nil
}
