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

	return ctrl.NewWebhookManagedBy(mgr, &servicesv1alpha1.ServiceConfiguration{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceconfiguration,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceconfigurations,verbs=create;update;delete,versions=v1alpha1,name=vserviceconfiguration.kb.io,admissionReviewVersions=v1

type serviceConfigurationWebhook struct {
	reader client.Reader
}

var _ admission.Validator[*servicesv1alpha1.ServiceConfiguration] = &serviceConfigurationWebhook{}

func serviceConfigurationCallerFromContext(ctx context.Context) validation.ServiceConfigurationCaller {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return validation.ServiceConfigurationCaller{}
	}
	return validation.ServiceConfigurationCaller{Username: req.UserInfo.Username}
}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceConfigurationWebhook) ValidateCreate(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) (admission.Warnings, error) {
	isDryRun := false
	if req, err := admission.RequestFromContext(ctx); err == nil && req.DryRun != nil {
		isDryRun = *req.DryRun
	}
	caller := serviceConfigurationCallerFromContext(ctx)

	serviceConfigurationLog.Info("validating create",
		"name", sc.GetName(),
		"serviceRef", sc.Spec.ServiceRef.Name,
		"isDryRun", isDryRun,
		"username", caller.Username,
	)

	if errs := validation.ValidateServiceConfigurationCreate(ctx, r.reader, sc, isDryRun, caller); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			sc.GetObjectKind().GroupVersionKind().GroupKind(),
			sc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceConfigurationWebhook) ValidateUpdate(ctx context.Context, oldSC, newSC *servicesv1alpha1.ServiceConfiguration) (admission.Warnings, error) {
	isDryRun := false
	if req, err := admission.RequestFromContext(ctx); err == nil && req.DryRun != nil {
		isDryRun = *req.DryRun
	}
	caller := serviceConfigurationCallerFromContext(ctx)

	serviceConfigurationLog.Info("validating update",
		"name", newSC.GetName(),
		"isDryRun", isDryRun,
		"username", caller.Username,
	)

	if errs := validation.ValidateServiceConfigurationUpdate(ctx, r.reader, oldSC, newSC, isDryRun, caller); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newSC.GetObjectKind().GroupVersionKind().GroupKind(),
			newSC.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today; the
// fan-out controller cascades billing-object cleanup via owner refs.
func (r *serviceConfigurationWebhook) ValidateDelete(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) (admission.Warnings, error) {
	serviceConfigurationLog.Info("validating delete", "name", sc.GetName())
	return nil, nil
}
