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

var meterDefinitionLog = logf.Log.WithName("meterdefinition-webhook")

// SetupMeterDefinitionWebhookWithManager registers the MeterDefinition
// webhook with the manager.
func SetupMeterDefinitionWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &meterDefinitionWebhook{
		client: mgr.GetClient(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.MeterDefinition{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-meterdefinition,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=meterdefinitions,verbs=create;update;delete,versions=v1alpha1,name=vmeterdefinition.kb.io,admissionReviewVersions=v1

type meterDefinitionWebhook struct {
	client client.Client
}

var _ admission.CustomValidator = &meterDefinitionWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *meterDefinitionWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	md, ok := obj.(*servicesv1alpha1.MeterDefinition)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	meterDefinitionLog.Info("validating create",
		"name", md.GetName(),
		"meterName", md.Spec.MeterName,
		"owner", md.Spec.Owner.Service,
	)

	opts := validation.MeterDefinitionValidationOptions{
		Context: ctx,
		Client:  r.client,
	}
	if errs := validation.ValidateMeterDefinitionCreate(md, opts); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			md.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *meterDefinitionWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldMD, ok := oldObj.(*servicesv1alpha1.MeterDefinition)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newMD, ok := newObj.(*servicesv1alpha1.MeterDefinition)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}
	meterDefinitionLog.Info("validating update", "name", newMD.GetName())

	opts := validation.MeterDefinitionValidationOptions{
		Context: ctx,
		Client:  r.client,
	}
	if errs := validation.ValidateMeterDefinitionUpdate(oldMD, newMD, opts); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newMD.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today.
func (r *meterDefinitionWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	md, ok := obj.(*servicesv1alpha1.MeterDefinition)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	meterDefinitionLog.Info("validating delete", "name", md.GetName())
	return nil, nil
}
