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

var serviceEntitlementLog = logf.Log.WithName("serviceentitlement-webhook")

// SetupServiceEntitlementWebhookWithManager registers the
// ServiceEntitlement validating webhook with the manager.
func SetupServiceEntitlementWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceEntitlementWebhook{
		// Use the API reader (uncached) so Service / parent-entitlement
		// lookups during admission don't depend on informer warm-up.
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceEntitlement{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceentitlement,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceentitlements,verbs=create;update;delete,versions=v1alpha1,name=vserviceentitlement.kb.io,admissionReviewVersions=v1

type serviceEntitlementWebhook struct {
	reader client.Reader
}

var _ admission.CustomValidator = &serviceEntitlementWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceEntitlementWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	se, ok := obj.(*servicesv1alpha1.ServiceEntitlement)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceEntitlementLog.Info("validating create",
		"name", se.GetName(),
		"serviceRef", se.Spec.ServiceRef.Name,
	)

	if errs := validation.ValidateServiceEntitlementCreate(ctx, r.reader, se); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			se.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceEntitlementWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSE, ok := oldObj.(*servicesv1alpha1.ServiceEntitlement)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newSE, ok := newObj.(*servicesv1alpha1.ServiceEntitlement)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}
	serviceEntitlementLog.Info("validating update", "name", newSE.GetName())

	if errs := validation.ValidateServiceEntitlementUpdate(ctx, r.reader, oldSE, newSE); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newSE.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. Refuses to delete a
// dependency entitlement while the entitlement that pulled it in is
// still Active.
func (r *serviceEntitlementWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	se, ok := obj.(*servicesv1alpha1.ServiceEntitlement)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceEntitlementLog.Info("validating delete",
		"name", se.GetName(),
		"origin", se.Status.Origin,
		"dependencyOf", se.Status.DependencyOf,
	)

	if errs := validation.ValidateServiceEntitlementDelete(ctx, r.reader, se); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			se.Name,
			errs,
		)
	}
	return nil, nil
}
