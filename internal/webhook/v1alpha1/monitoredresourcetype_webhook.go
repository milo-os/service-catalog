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

var monitoredResourceTypeLog = logf.Log.WithName("monitoredresourcetype-webhook")

// SetupMonitoredResourceTypeWebhookWithManager registers the
// MonitoredResourceType webhook with the manager.
func SetupMonitoredResourceTypeWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &monitoredResourceTypeWebhook{
		client: mgr.GetClient(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.MonitoredResourceType{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-monitoredresourcetype,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=monitoredresourcetypes,verbs=create;update;delete,versions=v1alpha1,name=vmonitoredresourcetype.kb.io,admissionReviewVersions=v1

type monitoredResourceTypeWebhook struct {
	client client.Client
}

var _ admission.CustomValidator = &monitoredResourceTypeWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *monitoredResourceTypeWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	mrt, ok := obj.(*servicesv1alpha1.MonitoredResourceType)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	monitoredResourceTypeLog.Info("validating create",
		"name", mrt.GetName(),
		"resourceTypeName", mrt.Spec.ResourceTypeName,
		"owner", mrt.Spec.Owner.Service,
	)

	opts := validation.MonitoredResourceTypeValidationOptions{
		Context: ctx,
		Client:  r.client,
	}
	if errs := validation.ValidateMonitoredResourceTypeCreate(mrt, opts); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			mrt.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *monitoredResourceTypeWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldMRT, ok := oldObj.(*servicesv1alpha1.MonitoredResourceType)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newMRT, ok := newObj.(*servicesv1alpha1.MonitoredResourceType)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}
	monitoredResourceTypeLog.Info("validating update", "name", newMRT.GetName())

	opts := validation.MonitoredResourceTypeValidationOptions{
		Context: ctx,
		Client:  r.client,
	}
	if errs := validation.ValidateMonitoredResourceTypeUpdate(oldMRT, newMRT, opts); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newMRT.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today.
func (r *monitoredResourceTypeWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	mrt, ok := obj.(*servicesv1alpha1.MonitoredResourceType)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	monitoredResourceTypeLog.Info("validating delete", "name", mrt.GetName())
	return nil, nil
}
