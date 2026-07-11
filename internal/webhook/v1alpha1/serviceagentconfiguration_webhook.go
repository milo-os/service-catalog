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

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/validation"
)

var serviceAgentConfigurationLog = logf.Log.WithName("serviceagentconfiguration-webhook")

// SetupServiceAgentConfigurationWebhookWithManager registers the
// ServiceAgentConfiguration webhook with the manager.
func SetupServiceAgentConfigurationWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceAgentConfigurationWebhook{
		// Use the API reader (uncached) so ServiceAgent/Service lookups
		// during admission don't block on informer sync — mirroring the
		// ServiceConfiguration webhook.
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceAgentConfiguration{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceagentconfiguration,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceagentconfigurations,verbs=create;update,versions=v1alpha1,name=vserviceagentconfiguration.kb.io,admissionReviewVersions=v1

type serviceAgentConfigurationWebhook struct {
	reader client.Reader
}

var _ admission.CustomValidator = &serviceAgentConfigurationWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceAgentConfigurationWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sac, ok := obj.(*servicesv1alpha1.ServiceAgentConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}

	isDryRun := false
	if req, err := admission.RequestFromContext(ctx); err == nil && req.DryRun != nil {
		isDryRun = *req.DryRun
	}

	serviceAgentConfigurationLog.Info("validating create",
		"name", sac.GetName(),
		"serviceAgentRef", sac.Spec.ServiceAgentRef.Name,
		"isDryRun", isDryRun,
	)

	if errs := validation.ValidateServiceAgentConfigurationCreate(ctx, r.reader, sac, isDryRun); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			sac.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceAgentConfigurationWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSAC, ok := oldObj.(*servicesv1alpha1.ServiceAgentConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newSAC, ok := newObj.(*servicesv1alpha1.ServiceAgentConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}

	isDryRun := false
	if req, err := admission.RequestFromContext(ctx); err == nil && req.DryRun != nil {
		isDryRun = *req.DryRun
	}

	serviceAgentConfigurationLog.Info("validating update", "name", newSAC.GetName(), "isDryRun", isDryRun)

	if errs := validation.ValidateServiceAgentConfigurationUpdate(ctx, r.reader, oldSAC, newSAC, isDryRun); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newSAC.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today; the
// AgentBinding reconciler re-selects the active version (or prunes)
// when a configuration goes away.
func (r *serviceAgentConfigurationWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sac, ok := obj.(*servicesv1alpha1.ServiceAgentConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceAgentConfigurationLog.Info("validating delete", "name", sac.GetName())
	return nil, nil
}
