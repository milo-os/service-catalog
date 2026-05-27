// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/validation"
)

var serviceConsumerLog = logf.Log.WithName("serviceconsumer-webhook")

// SetupServiceConsumerWebhookWithManager registers the ServiceConsumer
// validating webhook with the manager.
func SetupServiceConsumerWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceConsumerWebhook{Client: mgr.GetClient()}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceConsumer{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceconsumer,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceconsumers,verbs=create;update,versions=v1alpha1,name=vserviceconsumer.kb.io,admissionReviewVersions=v1

type serviceConsumerWebhook struct {
	Client client.Client
}

var _ admission.CustomValidator = &serviceConsumerWebhook{}

func userInfoFromContext(ctx context.Context) authenticationv1.UserInfo {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		serviceConsumerLog.V(1).Info("admission request not found in context, treating as non-controller caller")
		return authenticationv1.UserInfo{}
	}
	return req.UserInfo
}

// canApprove returns true when the caller has the "approve" verb on
// serviceconsumers. Callers with this privilege (e.g. the services controller
// whose cert carries system:masters) can update any spec field; all others are
// restricted to spec.approval only.
func (r *serviceConsumerWebhook) canApprove(ctx context.Context, user authenticationv1.UserInfo) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for k, v := range user.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user.Username,
			Groups: user.Groups,
			UID:    user.UID,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     "approve",
				Group:    "services.miloapis.com",
				Resource: "serviceconsumers",
			},
		},
	}
	if err := r.Client.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("SubjectAccessReview failed: %w", err)
	}
	return sar.Status.Allowed, nil
}

// ValidateCreate implements webhook.CustomValidator. Create access is
// governed entirely by RBAC; no additional validation needed.
func (r *serviceConsumerWebhook) ValidateCreate(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator. Callers with the
// "approve" verb on serviceconsumers may update any spec field; others are
// restricted to spec.approval.
func (r *serviceConsumerWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSC, ok := oldObj.(*servicesv1alpha1.ServiceConsumer)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}
	newSC, ok := newObj.(*servicesv1alpha1.ServiceConsumer)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}
	user := userInfoFromContext(ctx)
	serviceConsumerLog.Info("validating update", "name", newSC.GetName(), "user", user.Username)

	privileged, err := r.canApprove(ctx, user)
	if err != nil {
		return nil, err
	}

	if errs := validation.ValidateServiceConsumerUpdate(privileged, oldSC, newSC); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			newObj.GetObjectKind().GroupVersionKind().GroupKind(),
			newSC.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. No-op today; the
// services controller drives ServiceConsumer lifecycle via owner refs.
func (r *serviceConsumerWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sc, ok := obj.(*servicesv1alpha1.ServiceConsumer)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	serviceConsumerLog.Info("validating delete", "name", sc.GetName())
	return nil, nil
}
