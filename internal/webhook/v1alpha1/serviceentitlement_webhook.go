// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"fmt"
	"reflect"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/validation"
)

var serviceEntitlementLog = logf.Log.WithName("serviceentitlement-webhook")

// SetupServiceEntitlementWebhookWithManager registers the
// ServiceEntitlement validating webhook with the manager.
func SetupServiceEntitlementWebhookWithManager(mgr ctrl.Manager, mcMgr mcmanager.Manager) error {
	webhook := &serviceEntitlementWebhook{
		// Use the API reader (uncached) so Service / parent-entitlement
		// lookups during admission don't depend on informer warm-up.
		reader: mgr.GetAPIReader(),
		mcMgr:  mcMgr,
	}

	return ctrl.NewWebhookManagedBy(mgr, &servicesv1alpha1.ServiceEntitlement{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceentitlement,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceentitlements,verbs=create;update;delete,versions=v1alpha1,name=vserviceentitlement.kb.io,admissionReviewVersions=v1

type serviceEntitlementWebhook struct {
	reader client.Reader
	mcMgr  mcmanager.Manager
}

var _ admission.Validator[*servicesv1alpha1.ServiceEntitlement] = &serviceEntitlementWebhook{}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceEntitlementWebhook) ValidateCreate(ctx context.Context, se *servicesv1alpha1.ServiceEntitlement) (admission.Warnings, error) {
	serviceEntitlementLog.Info("validating create",
		"name", se.GetName(),
		"serviceRef", se.Spec.ServiceRef.Name,
	)

	errs := validation.ValidateServiceEntitlementCreate(ctx, r.reader, se)
	if se.Spec.ProviderActivation != nil {
		allowed, err := r.callerCanManage(ctx, userInfoFromContext(ctx))
		if err != nil {
			return nil, err
		}
		if !allowed {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "providerActivation"), "only the services controller may record provider activation provenance"))
		}
	}
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			se.GetObjectKind().GroupVersionKind().GroupKind(),
			se.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (r *serviceEntitlementWebhook) ValidateUpdate(ctx context.Context, oldSE, newSE *servicesv1alpha1.ServiceEntitlement) (admission.Warnings, error) {
	serviceEntitlementLog.Info("validating update", "name", newSE.GetName())

	errs := validation.ValidateServiceEntitlementUpdate(ctx, r.reader, oldSE, newSE)
	if !reflect.DeepEqual(oldSE.Spec.ProviderActivation, newSE.Spec.ProviderActivation) {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "providerActivation"), "provider activation provenance is immutable"))
	}
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newSE.GetObjectKind().GroupVersionKind().GroupKind(),
			newSE.Name,
			errs,
		)
	}
	return nil, nil
}

func (r *serviceEntitlementWebhook) callerCanManage(ctx context.Context, user authenticationv1.UserInfo) (bool, error) {
	if r.mcMgr == nil {
		return false, fmt.Errorf("can't verify permission to record provider activation right now")
	}
	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok || clusterName == "" {
		return false, fmt.Errorf("can't determine which consumer project this entitlement targets")
	}
	cl, err := r.mcMgr.GetCluster(ctx, clusterName)
	if err != nil {
		return false, fmt.Errorf("can't reach project %q to verify entitlement permissions: %w", clusterName, err)
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: "manage", Group: servicesv1alpha1.GroupVersion.Group, Resource: "serviceentitlements",
		},
		User: user.Username, Groups: user.Groups, UID: user.UID, Extra: convertExtra(user.Extra),
	}}
	if err := cl.GetClient().Create(ctx, sar); err != nil {
		return false, fmt.Errorf("couldn't verify entitlement permissions in project %q: %w", clusterName, err)
	}
	return sar.Status.Allowed, nil
}

// ValidateDelete implements webhook.CustomValidator. Refuses to delete a
// dependency entitlement while the entitlement that pulled it in is
// still Active.
func (r *serviceEntitlementWebhook) ValidateDelete(ctx context.Context, se *servicesv1alpha1.ServiceEntitlement) (admission.Warnings, error) {
	serviceEntitlementLog.Info("validating delete",
		"name", se.GetName(),
		"origin", se.Status.Origin,
		"dependencyOf", se.Status.DependencyOf,
	)

	if errs := validation.ValidateServiceEntitlementDelete(ctx, r.reader, se); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			se.GetObjectKind().GroupVersionKind().GroupKind(),
			se.Name,
			errs,
		)
	}
	return nil, nil
}
