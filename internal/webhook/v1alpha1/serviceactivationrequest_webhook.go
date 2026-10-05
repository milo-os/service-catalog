// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"fmt"
	"reflect"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// SetupServiceActivationRequestWebhookWithManager registers admission for the
// provider-side activation command. Authorization is evaluated at admission
// with the original caller identity and snapshotted on the request for the
// asynchronous reconciler.
func SetupServiceActivationRequestWebhookWithManager(mgr ctrl.Manager, mcMgr mcmanager.Manager) error {
	webhook := &serviceActivationRequestWebhook{reader: mgr.GetAPIReader(), mcMgr: mcMgr}
	return ctrl.NewWebhookManagedBy(mgr, &servicesv1alpha1.ServiceActivationRequest{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-services-miloapis-com-v1alpha1-serviceactivationrequest,mutating=true,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceactivationrequests,verbs=create,versions=v1alpha1,name=mserviceactivationrequest.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-services-miloapis-com-v1alpha1-serviceactivationrequest,mutating=false,failurePolicy=fail,sideEffects=None,groups=services.miloapis.com,resources=serviceactivationrequests,verbs=create;update,versions=v1alpha1,name=vserviceactivationrequest.kb.io,admissionReviewVersions=v1

type serviceActivationRequestWebhook struct {
	reader client.Reader
	mcMgr  mcmanager.Manager
}

var _ admission.Defaulter[*servicesv1alpha1.ServiceActivationRequest] = &serviceActivationRequestWebhook{}
var _ admission.Validator[*servicesv1alpha1.ServiceActivationRequest] = &serviceActivationRequestWebhook{}

func (w *serviceActivationRequestWebhook) Default(ctx context.Context, request *servicesv1alpha1.ServiceActivationRequest) error {
	admissionRequest, err := admission.RequestFromContext(ctx)
	if err != nil {
		return fmt.Errorf("can't identify who requested this activation: %w", err)
	}
	request.Spec.RequestedBy = servicesv1alpha1.ActorReference{
		Username: admissionRequest.UserInfo.Username,
		UID:      types.UID(admissionRequest.UserInfo.UID),
	}
	if request.Annotations == nil {
		request.Annotations = map[string]string{}
	}

	allowed, reason, err := w.authorize(ctx, admissionRequest.UserInfo, request)
	if err != nil {
		return err
	}
	request.Annotations[servicesv1alpha1.ServiceActivationAuthorizedAnnotation] = fmt.Sprintf("%t", allowed)
	request.Annotations[servicesv1alpha1.ServiceActivationAuthorizationReasonAnnotation] = reason
	return nil
}

func (w *serviceActivationRequestWebhook) authorize(ctx context.Context, user authenticationv1.UserInfo, request *servicesv1alpha1.ServiceActivationRequest) (bool, string, error) {
	var svc servicesv1alpha1.Service
	if err := w.reader.Get(ctx, types.NamespacedName{Name: request.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "ServiceNotFound", nil
		}
		return false, "", fmt.Errorf("can't resolve service %q: %w", request.Spec.ServiceRef.Name, err)
	}
	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		return false, "ServiceNotPublished", nil
	}

	providerProject, ok := mccontext.ClusterFrom(ctx)
	if !ok || providerProject == "" {
		return false, "", fmt.Errorf("can't determine which provider project this request targets")
	}
	if svc.Spec.Owner.ProducerProjectRef.Name != string(providerProject) {
		return false, "ServiceOwnedByAnotherProject", nil
	}
	if request.Spec.ConsumerProjectRef.Name == "" {
		return false, "ConsumerProjectRequired", nil
	}
	if w.mcMgr == nil {
		return false, "", fmt.Errorf("can't verify consumer authorization right now; please try again")
	}
	consumerCluster, err := w.mcMgr.GetCluster(ctx, multicluster.ClusterName(request.Spec.ConsumerProjectRef.Name))
	if err != nil {
		return false, "", fmt.Errorf("can't reach consumer project %q to verify authorization: %w", request.Spec.ConsumerProjectRef.Name, err)
	}

	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb:     "activate",
			Group:    servicesv1alpha1.GroupVersion.Group,
			Resource: "serviceentitlements",
		},
		User: user.Username, Groups: user.Groups, UID: user.UID, Extra: convertExtra(user.Extra),
	}}
	if err := consumerCluster.GetClient().Create(ctx, sar); err != nil {
		return false, "", fmt.Errorf("couldn't verify authorization in consumer project %q: %w", request.Spec.ConsumerProjectRef.Name, err)
	}
	if !sar.Status.Allowed {
		return false, "ConsumerNotOptedIn", nil
	}
	return true, "ConsumerPolicyAllowed", nil
}

func (w *serviceActivationRequestWebhook) ValidateCreate(_ context.Context, request *servicesv1alpha1.ServiceActivationRequest) (admission.Warnings, error) {
	var errs field.ErrorList
	if request.Spec.RequestedBy.Username == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "requestedBy", "username"), "must be populated from the authenticated caller"))
	}
	if request.Annotations[servicesv1alpha1.ServiceActivationAuthorizedAnnotation] == "" || request.Annotations[servicesv1alpha1.ServiceActivationAuthorizationReasonAnnotation] == "" {
		errs = append(errs, field.Required(field.NewPath("metadata", "annotations"), "authorization must be evaluated before the request is stored"))
	}
	return activationValidationResult(request, errs)
}

func (w *serviceActivationRequestWebhook) ValidateUpdate(_ context.Context, oldRequest, newRequest *servicesv1alpha1.ServiceActivationRequest) (admission.Warnings, error) {
	var errs field.ErrorList
	if !reflect.DeepEqual(oldRequest.Spec, newRequest.Spec) {
		errs = append(errs, field.Forbidden(field.NewPath("spec"), "an activation request is immutable; create a new request instead"))
	}
	for _, key := range []string{servicesv1alpha1.ServiceActivationAuthorizedAnnotation, servicesv1alpha1.ServiceActivationAuthorizationReasonAnnotation} {
		if oldRequest.Annotations[key] != newRequest.Annotations[key] {
			errs = append(errs, field.Forbidden(field.NewPath("metadata", "annotations").Key(key), "the admission authorization decision is immutable"))
		}
	}
	return activationValidationResult(newRequest, errs)
}

func (w *serviceActivationRequestWebhook) ValidateDelete(context.Context, *servicesv1alpha1.ServiceActivationRequest) (admission.Warnings, error) {
	return nil, nil
}

func activationValidationResult(request *servicesv1alpha1.ServiceActivationRequest, errs field.ErrorList) (admission.Warnings, error) {
	if len(errs) == 0 {
		return nil, nil
	}
	return nil, apierrors.NewInvalid(request.GroupVersionKind().GroupKind(), request.Name, errs)
}
