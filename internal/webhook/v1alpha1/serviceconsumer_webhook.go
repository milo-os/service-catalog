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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/validation"
)

var serviceConsumerLog = logf.Log.WithName("serviceconsumer-webhook")

// SetupServiceConsumerWebhookWithManager registers the ServiceConsumer
// validating webhook with the manager. mcMgr is the multicluster manager used
// to reach the project control plane a request targets so the webhook can
// authorize the caller there via a SubjectAccessReview.
func SetupServiceConsumerWebhookWithManager(mgr ctrl.Manager, mcMgr mcmanager.Manager) error {
	webhook := &serviceConsumerWebhook{mcMgr: mcMgr}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&servicesv1alpha1.ServiceConsumer{}).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceconsumer,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceconsumers,verbs=create;update,versions=v1alpha1,name=vserviceconsumer.kb.io,admissionReviewVersions=v1

type serviceConsumerWebhook struct {
	mcMgr mcmanager.Manager
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

// callerCanManage reports whether the admission caller holds the
// serviceconsumers.manage permission in the project control plane the request
// targets. It issues a SubjectAccessReview against the originating project
// cluster (identified by the cluster context the ClusterAwareServer injects
// from the request's UserInfo.Extra), rather than inspecting the username —
// the controller's identity is determined by what it is authorized to do, not
// by how it happens to be named. Fails closed: any inability to evaluate the
// permission returns an error so the caller is not silently granted manage.
func (r *serviceConsumerWebhook) callerCanManage(ctx context.Context, user authenticationv1.UserInfo) (bool, error) {
	if r.mcMgr == nil {
		return false, fmt.Errorf("can't verify your permissions to change this service consumer right now; please try again")
	}
	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok || clusterName == "" {
		return false, fmt.Errorf("can't determine which project this request targets, so your permissions can't be verified")
	}
	cl, err := r.mcMgr.GetCluster(ctx, clusterName)
	if err != nil {
		serviceConsumerLog.Error(err, "failed to reach project control plane for ServiceConsumer authorization", "cluster", clusterName)
		return false, fmt.Errorf("can't reach project %q to verify your permissions right now; please try again", clusterName)
	}

	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     "manage",
				Group:    servicesv1alpha1.GroupVersion.Group,
				Resource: "serviceconsumers",
			},
			User:   user.Username,
			Groups: user.Groups,
			UID:    user.UID,
			Extra:  convertExtra(user.Extra),
		},
	}
	if err := cl.GetClient().Create(ctx, sar); err != nil {
		serviceConsumerLog.Error(err, "failed to evaluate SubjectAccessReview for ServiceConsumer authorization", "cluster", clusterName)
		return false, fmt.Errorf("couldn't verify your permissions in project %q right now; please try again", clusterName)
	}
	return sar.Status.Allowed, nil
}

// convertExtra adapts authentication ExtraValues (as carried on the admission
// request's UserInfo) to the authorization ExtraValues a SubjectAccessReview
// expects, preserving the caller's full identity for the authorizer.
func convertExtra(in map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]authorizationv1.ExtraValue, len(in))
	for k, v := range in {
		out[k] = authorizationv1.ExtraValue(v)
	}
	return out
}

// ValidateCreate implements webhook.CustomValidator. ServiceConsumer objects
// are controller-managed; reject creates from callers without manage.
func (r *serviceConsumerWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sc, ok := obj.(*servicesv1alpha1.ServiceConsumer)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	user := userInfoFromContext(ctx)
	serviceConsumerLog.Info("validating create", "name", sc.GetName(), "user", user.Username)

	canManage, err := r.callerCanManage(ctx, user)
	if err != nil {
		return nil, err
	}

	if errs := validation.ValidateServiceConsumerCreate(canManage, sc); len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			sc.Name,
			errs,
		)
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator. Callers without manage may
// only touch spec.approval; the controller has full write access.
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

	canManage, err := r.callerCanManage(ctx, user)
	if err != nil {
		return nil, err
	}

	if errs := validation.ValidateServiceConsumerUpdate(canManage, oldSC, newSC); len(errs) > 0 {
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
