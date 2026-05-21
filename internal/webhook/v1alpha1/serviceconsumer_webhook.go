// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
	"go.miloapis.com/services/internal/validation"
)

var serviceConsumerLog = logf.Log.WithName("serviceconsumer-webhook")

// SetupServiceConsumerWebhookWithManager registers the ServiceConsumer
// validating webhook with the manager.
func SetupServiceConsumerWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &serviceConsumerWebhook{}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceConsumer{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceconsumer,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceconsumers,verbs=create;update,versions=v1alpha1,name=vserviceconsumer.kb.io,admissionReviewVersions=v1

type serviceConsumerWebhook struct{}

var _ admission.CustomValidator = &serviceConsumerWebhook{}

func userInfoFromContext(ctx context.Context) authenticationv1.UserInfo {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		serviceConsumerLog.V(1).Info("admission request not found in context, treating as non-controller caller")
		return authenticationv1.UserInfo{}
	}
	return req.UserInfo
}

// ValidateCreate implements webhook.CustomValidator. ServiceConsumer
// objects are controller-managed; reject creates from any other caller.
func (r *serviceConsumerWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sc, ok := obj.(*servicesv1alpha1.ServiceConsumer)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	user := userInfoFromContext(ctx)
	serviceConsumerLog.Info("validating create", "name", sc.GetName(), "user", user.Username)

	if errs := validation.ValidateServiceConsumerCreate(user, sc); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			sc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator. Providers may only
// touch spec.approval; the controller has full write access.
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

	if errs := validation.ValidateServiceConsumerUpdate(user, oldSC, newSC); len(errs) > 0 {
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
