// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/validation"
)

var serviceEntitlementLog = logf.Log.WithName("serviceentitlement-webhook")

// requesterAPIGroup and requesterKind are the fixed subject reference values
// stamped onto ServiceEntitlement.spec.requestedBy. Every ServiceEntitlement
// is created within a specific human user's request context (directly, or by
// this controller acting on their behalf for a dependency), so the subject
// kind is always iam.miloapis.com/User.
const (
	requesterAPIGroup = "iam.miloapis.com"
	requesterKind     = "User"
)

// SetupServiceEntitlementWebhookWithManager registers the
// ServiceEntitlement mutating and validating webhooks with the manager.
func SetupServiceEntitlementWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceEntitlementWebhook{
		// Use the API reader (uncached) so Service / parent-entitlement
		// lookups during admission don't depend on informer warm-up.
		reader: mgr.GetAPIReader(),
	}

	return ctrl.NewWebhookManagedBy(mgr, &servicesv1alpha1.ServiceEntitlement{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-services-miloapis-com-v1alpha1-serviceentitlement,mutating=true,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceentitlements,verbs=create,versions=v1alpha1,name=mserviceentitlement.kb.io,admissionReviewVersions=v1

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceentitlement,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceentitlements,verbs=create;update;delete,versions=v1alpha1,name=vserviceentitlement.kb.io,admissionReviewVersions=v1

type serviceEntitlementWebhook struct {
	reader client.Reader
}

var _ admission.Defaulter[*servicesv1alpha1.ServiceEntitlement] = &serviceEntitlementWebhook{}
var _ admission.Validator[*servicesv1alpha1.ServiceEntitlement] = &serviceEntitlementWebhook{}

// Default implements webhook.CustomDefaulter. It stamps spec.requestedBy from
// the create request's caller identity, so downstream consumers (for example,
// CRM contact-group enrollment) can resolve who asked for this entitlement
// without reconstructing it from audit logs. The mutating webhook is
// registered for "create" only (see the marker above), so this never
// overwrites an existing value on update.
//
// A value already set on the incoming object (for example, a dependency
// entitlement the reconciler creates on a consumer's behalf — see
// ServiceEntitlementReconciler.ensureDependencies, which copies the parent's
// requestedBy directly) is left alone rather than replaced with the
// controller's own identity.
//
// Left unset when the caller isn't a human: a request with no admission
// context (for example, envtest calls made directly against the API server),
// no UserInfo.UID, or a system:-prefixed username (service accounts,
// impersonated system identities) stamps nothing. A ServiceEntitlement
// created this way simply has no requestedBy, which downstream consumers must
// already treat as "requester unknown."
func (r *serviceEntitlementWebhook) Default(ctx context.Context, se *servicesv1alpha1.ServiceEntitlement) error {
	if se.Spec.RequestedBy != nil {
		return nil
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		serviceEntitlementLog.V(1).Info("admission request not found in context, skipping requestedBy defaulting", "name", se.GetName())
		return nil
	}

	requester := requesterFromUserInfo(req.UserInfo)
	if requester == nil {
		return nil
	}

	serviceEntitlementLog.Info("stamping requestedBy", "name", se.GetName(), "requester", requester.Name)
	se.Spec.RequestedBy = requester
	return nil
}

// requesterFromUserInfo builds a RequesterRef from an admission request's
// caller identity, or nil if the caller doesn't look like a human. UserInfo.UID
// is used as the requester's Name because for a Milo User it is the User's
// metadata.name — the same join key Milo's own Contact ownership validator
// compares against Contact.spec.subject.name.
func requesterFromUserInfo(u authenticationv1.UserInfo) *servicesv1alpha1.RequesterRef {
	if u.UID == "" {
		return nil
	}
	if strings.HasPrefix(u.Username, "system:") {
		return nil
	}
	return &servicesv1alpha1.RequesterRef{
		APIGroup: requesterAPIGroup,
		Kind:     requesterKind,
		Name:     u.UID,
		Email:    u.Username,
	}
}

// ValidateCreate implements webhook.CustomValidator.
func (r *serviceEntitlementWebhook) ValidateCreate(ctx context.Context, se *servicesv1alpha1.ServiceEntitlement) (admission.Warnings, error) {
	serviceEntitlementLog.Info("validating create",
		"name", se.GetName(),
		"serviceRef", se.Spec.ServiceRef.Name,
	)

	if errs := validation.ValidateServiceEntitlementCreate(ctx, r.reader, se); len(errs) > 0 {
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

	if errs := validation.ValidateServiceEntitlementUpdate(ctx, r.reader, oldSE, newSE); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newSE.GetObjectKind().GroupVersionKind().GroupKind(),
			newSE.Name,
			errs,
		)
	}
	return nil, nil
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
