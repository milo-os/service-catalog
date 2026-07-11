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

var serviceAgentLog = logf.Log.WithName("serviceagent-webhook")

// SetupServiceAgentWebhookWithManager registers the ServiceAgent
// webhook with the manager.
func SetupServiceAgentWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceAgentWebhook{
		// Use the API reader (uncached) so Service lookups during
		// admission don't block on informer sync — mirroring the
		// ServiceConfiguration webhook.
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceAgent{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceagent,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceagents,verbs=create;update,versions=v1alpha1,name=vserviceagent.kb.io,admissionReviewVersions=v1

type serviceAgentWebhook struct {
	reader client.Reader
}

var _ admission.CustomValidator = &serviceAgentWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceAgentWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sa, ok := obj.(*servicesv1alpha1.ServiceAgent)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}

	isDryRun := false
	if req, err := admission.RequestFromContext(ctx); err == nil && req.DryRun != nil {
		isDryRun = *req.DryRun
	}

	serviceAgentLog.Info("validating create",
		"name", sa.GetName(),
		"serviceRef", sa.Spec.ServiceRef.Name,
		"isDryRun", isDryRun,
	)

	if errs := validation.ValidateServiceAgentCreate(ctx, r.reader, sa, isDryRun); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			sa.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceAgentWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSA, ok := oldObj.(*servicesv1alpha1.ServiceAgent)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newSA, ok := newObj.(*servicesv1alpha1.ServiceAgent)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}

	serviceAgentLog.Info("validating update", "name", newSA.GetName())

	if errs := validation.ValidateServiceAgentUpdate(oldSA, newSA); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newSA.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today; the
// AgentBinding reconciler prunes projections when the agent goes away.
func (r *serviceAgentWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sa, ok := obj.(*servicesv1alpha1.ServiceAgent)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceAgentLog.Info("validating delete", "name", sa.GetName())
	return nil, nil
}
