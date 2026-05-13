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

var serviceConfigurationLog = logf.Log.WithName("serviceconfiguration-webhook")

// SetupServiceConfigurationWebhookWithManager registers the
// ServiceConfiguration webhook with the manager.
func SetupServiceConfigurationWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceConfigurationWebhook{
		// Use the API reader (uncached) so Service lookups during admission
		// don't block on informer sync — the cache for Service may not be
		// ready when the first ServiceConfiguration admission arrives.
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceConfiguration{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceconfiguration,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceconfigurations,verbs=create;update;delete,versions=v1alpha1,name=vserviceconfiguration.kb.io,admissionReviewVersions=v1

type serviceConfigurationWebhook struct {
	reader client.Reader
}

var _ admission.CustomValidator = &serviceConfigurationWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceConfigurationWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sc, ok := obj.(*servicesv1alpha1.ServiceConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceConfigurationLog.Info("validating create",
		"name", sc.GetName(),
		"serviceRef", sc.Spec.ServiceRef.Name,
	)

	if errs := validation.ValidateServiceConfigurationCreate(ctx, r.reader, sc); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			sc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceConfigurationWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSC, ok := oldObj.(*servicesv1alpha1.ServiceConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newSC, ok := newObj.(*servicesv1alpha1.ServiceConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}
	serviceConfigurationLog.Info("validating update", "name", newSC.GetName())

	if errs := validation.ValidateServiceConfigurationUpdate(ctx, r.reader, oldSC, newSC); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newSC.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today; the
// fan-out controller cascades billing-object cleanup via owner refs.
func (r *serviceConfigurationWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sc, ok := obj.(*servicesv1alpha1.ServiceConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceConfigurationLog.Info("validating delete", "name", sc.GetName())
	return nil, nil
}
