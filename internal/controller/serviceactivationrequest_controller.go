// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	activationRequestPollInterval = 30 * time.Second

	conditionTypeServiceOwned       = "ServiceOwned"
	conditionTypeAuthorized         = "Authorized"
	conditionTypeEntitlementCreated = "EntitlementCreated"
	conditionTypeActivationReady    = "Ready"
)

// ServiceActivationRequestReconciler turns an authorized, provider-side
// request into the ordinary ServiceEntitlement consumed by the existing
// enablement pipeline. Requests are one-shot: once status records an
// entitlement, its later absence means the consumer disabled it and must not
// cause a replacement entitlement to be created.
type ServiceActivationRequestReconciler struct {
	rootClient client.Reader
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceactivationrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceactivationrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch;create

func (r *ServiceActivationRequestReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	providerProject := string(req.ClusterName)
	if providerProject == "" {
		return ctrl.Result{}, fmt.Errorf("ServiceActivationRequest reconcile invoked without a cluster name")
	}

	providerCluster, err := r.Manager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get provider cluster %q: %w", providerProject, err)
	}
	providerClient := providerCluster.GetClient()

	var activation servicesv1alpha1.ServiceActivationRequest
	if err := providerClient.Get(ctx, req.NamespacedName, &activation); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceActivationRequest: %w", err)
	}
	logger := log.FromContext(ctx).WithValues("providerProject", providerProject, "activationRequest", activation.Name)
	ctx = log.IntoContext(ctx, logger)

	if !activation.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	svc, err := r.resolveService(ctx, activation.Spec.ServiceRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setFailed(ctx, providerClient, &activation, "ServiceNotFound", "The requested service could not be found.")
		}
		return ctrl.Result{}, fmt.Errorf("failed to resolve Service %q: %w", activation.Spec.ServiceRef.Name, err)
	}
	if svc.Spec.Owner.ProducerProjectRef.Name != providerProject {
		setActivationCondition(&activation, conditionTypeServiceOwned, metav1.ConditionFalse, "ServiceOwnedByAnotherProject",
			fmt.Sprintf("Service %q is not owned by provider project %q.", svc.Spec.ServiceName, providerProject))
		return ctrl.Result{}, r.setFailed(ctx, providerClient, &activation, "ServiceOwnedByAnotherProject", "The provider project does not own the requested service.")
	}
	setActivationCondition(&activation, conditionTypeServiceOwned, metav1.ConditionTrue, "ServiceOwned", "The provider project owns the requested service.")

	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		return ctrl.Result{}, r.setFailed(ctx, providerClient, &activation, "ServiceNotPublished", "The requested service is not published.")
	}

	if activation.Annotations[servicesv1alpha1.ServiceActivationAuthorizedAnnotation] != servicesv1alpha1.ServiceActivationAuthorizedValueTrue {
		reason := activation.Annotations[servicesv1alpha1.ServiceActivationAuthorizationReasonAnnotation]
		if reason == "" {
			reason = "ConsumerNotOptedIn"
		}
		setActivationCondition(&activation, conditionTypeAuthorized, metav1.ConditionFalse, reason,
			"The consumer project has not authorized this provider actor to activate services.")
		return ctrl.Result{}, r.updateStatus(ctx, providerClient, &activation, servicesv1alpha1.ServiceActivationRequestPhaseDenied)
	}
	setActivationCondition(&activation, conditionTypeAuthorized, metav1.ConditionTrue, "ConsumerPolicyAllowed",
		"Consumer policy authorized this provider actor to activate the service.")

	consumerProject := activation.Spec.ConsumerProjectRef.Name
	consumerCluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(consumerProject))
	if err != nil {
		logger.Info("consumer cluster not yet available, requeuing", "consumerProject", consumerProject, "err", err)
		return ctrl.Result{RequeueAfter: activationRequestPollInterval}, nil
	}
	consumerClient := consumerCluster.GetClient()

	entitlementName := svc.Name
	if activation.Status.EntitlementRef != nil && activation.Status.EntitlementRef.Name != "" {
		entitlementName = activation.Status.EntitlementRef.Name
	}

	var entitlement servicesv1alpha1.ServiceEntitlement
	err = consumerClient.Get(ctx, types.NamespacedName{Name: entitlementName}, &entitlement)
	if apierrors.IsNotFound(err) {
		if activation.Status.EntitlementRef != nil {
			setActivationCondition(&activation, conditionTypeActivationReady, metav1.ConditionFalse, "ConsumerDisabled",
				"The consumer removed the service entitlement. This one-shot request will not recreate it.")
			return ctrl.Result{}, r.updateStatus(ctx, providerClient, &activation, servicesv1alpha1.ServiceActivationRequestPhaseDisabled)
		}

		entitlement = servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: entitlementName},
			Spec: servicesv1alpha1.ServiceEntitlementSpec{
				ServiceRef:     servicesv1alpha1.ServiceRef{Name: activation.Spec.ServiceRef.Name},
				RequestMessage: activation.Spec.RequestMessage,
				ProviderActivation: &servicesv1alpha1.ProviderActivation{
					RequestRef: servicesv1alpha1.ServiceActivationRequestReference{
						Name: activation.Name,
						UID:  activation.UID,
					},
					ProviderProjectRef: servicesv1alpha1.ProducerProjectReference{Name: providerProject},
					Actor:              activation.Spec.RequestedBy,
					RequestedAt:        activation.CreationTimestamp,
				},
			},
		}
		if err := consumerClient.Create(ctx, &entitlement); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("failed to create ServiceEntitlement %q in consumer project %q: %w", entitlementName, consumerProject, err)
			}
			if err := consumerClient.Get(ctx, types.NamespacedName{Name: entitlementName}, &entitlement); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get concurrently-created ServiceEntitlement %q: %w", entitlementName, err)
			}
		}
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceEntitlement %q from consumer project %q: %w", entitlementName, consumerProject, err)
	}
	if activation.Status.EntitlementRef != nil && activation.Status.EntitlementRef.UID != "" &&
		entitlement.UID != activation.Status.EntitlementRef.UID {
		setActivationCondition(&activation, conditionTypeActivationReady, metav1.ConditionFalse, "ConsumerDisabled",
			"The original entitlement no longer exists. This one-shot request will not adopt or recreate it.")
		return ctrl.Result{}, r.updateStatus(ctx, providerClient, &activation, servicesv1alpha1.ServiceActivationRequestPhaseDisabled)
	}

	activation.Status.EntitlementRef = &servicesv1alpha1.ServiceEntitlementReference{Name: entitlement.Name, UID: entitlement.UID}
	setActivationCondition(&activation, conditionTypeEntitlementCreated, metav1.ConditionTrue, "EntitlementCreated",
		"The consumer-side service entitlement exists.")

	phase := servicesv1alpha1.ServiceActivationRequestPhasePending
	readyStatus := metav1.ConditionFalse
	readyReason := "EntitlementPending"
	readyMessage := "Waiting for the consumer-side service entitlement to become active."
	switch entitlement.Status.Phase {
	case servicesv1alpha1.EntitlementPhaseActive:
		phase = servicesv1alpha1.ServiceActivationRequestPhaseActive
		readyStatus = metav1.ConditionTrue
		readyReason = "EntitlementActive"
		readyMessage = "The service is active in the consumer project."
	case servicesv1alpha1.EntitlementPhaseRejected:
		phase = servicesv1alpha1.ServiceActivationRequestPhaseFailed
		readyReason = "EntitlementRejected"
		readyMessage = "The consumer-side service entitlement was rejected."
	}
	setActivationCondition(&activation, conditionTypeActivationReady, readyStatus, readyReason, readyMessage)
	if err := r.updateStatus(ctx, providerClient, &activation, phase); err != nil {
		return ctrl.Result{}, err
	}
	if phase == servicesv1alpha1.ServiceActivationRequestPhasePending {
		return ctrl.Result{RequeueAfter: activationRequestPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ServiceActivationRequestReconciler) resolveService(ctx context.Context, canonicalName string) (*servicesv1alpha1.Service, error) {
	var services servicesv1alpha1.ServiceList
	if err := r.rootClient.List(ctx, &services); err != nil {
		return nil, fmt.Errorf("failed to list Services to resolve spec.serviceName %q: %w", canonicalName, err)
	}
	for i := range services.Items {
		if services.Items[i].Spec.ServiceName == canonicalName {
			return &services.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(servicesv1alpha1.GroupVersion.WithResource("services").GroupResource(), canonicalName)
}

func (r *ServiceActivationRequestReconciler) setFailed(ctx context.Context, c client.Client, activation *servicesv1alpha1.ServiceActivationRequest, reason, message string) error {
	setActivationCondition(activation, conditionTypeActivationReady, metav1.ConditionFalse, reason, message)
	return r.updateStatus(ctx, c, activation, servicesv1alpha1.ServiceActivationRequestPhaseFailed)
}

func (r *ServiceActivationRequestReconciler) updateStatus(ctx context.Context, c client.Client, activation *servicesv1alpha1.ServiceActivationRequest, phase servicesv1alpha1.ServiceActivationRequestPhase) error {
	activation.Status.Phase = phase
	activation.Status.ObservedGeneration = activation.Generation
	var persisted servicesv1alpha1.ServiceActivationRequest
	if err := c.Get(ctx, client.ObjectKeyFromObject(activation), &persisted); err != nil {
		return fmt.Errorf("failed to refresh ServiceActivationRequest before status update: %w", err)
	}
	if apiequality.Semantic.DeepEqual(&persisted.Status, &activation.Status) {
		return nil
	}
	activation.ResourceVersion = persisted.ResourceVersion
	if err := c.Status().Update(ctx, activation); err != nil {
		return fmt.Errorf("failed to update ServiceActivationRequest status: %w", err)
	}
	return nil
}

func setActivationCondition(activation *servicesv1alpha1.ServiceActivationRequest, conditionType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&activation.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: activation.Generation,
	})
}

// SetupWithManager registers the reconciler on every engaged project cluster.
// Requests are created in provider project control planes; the referenced
// consumer project is reached through the multicluster manager during
// reconciliation.
func (r *ServiceActivationRequestReconciler) SetupWithManager(mcMgr mcmanager.Manager, rootMgr ctrl.Manager) error {
	// Activation requests are created immediately after a Service is published.
	// Use the uncached reader so canonical-name resolution cannot race the root
	// cache, then match only the immutable spec.serviceName.
	r.rootClient = rootMgr.GetAPIReader()
	r.Manager = mcMgr

	return mcbuilder.ControllerManagedBy(mcMgr).
		Named("service-activation-request").
		For(&servicesv1alpha1.ServiceActivationRequest{}, mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(r)
}
