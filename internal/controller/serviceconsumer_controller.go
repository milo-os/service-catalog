// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

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
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const (
	reasonConsumerApproved = "ConsumerApproved"
	reasonConsumerDenied   = "ConsumerDenied"
	reasonConsumerPending  = "ConsumerPending"
)

// ServiceConsumerReconciler runs in every engaged project cluster. Each
// reconcile call carries the *provider* project name as req.ClusterName. The
// reconciler reacts to provider approval decisions (spec.approval) and
// propagates the result onto the matching ServiceEntitlement in the consumer
// project's virtual control plane.
type ServiceConsumerReconciler struct {
	Manager mcmanager.Manager
	Scheme  *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/status,verbs=get;update;patch

func (r *ServiceConsumerReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	providerProject := req.ClusterName
	if providerProject == "" {
		return ctrl.Result{}, fmt.Errorf("ServiceConsumer reconcile invoked without a cluster name")
	}

	providerCluster, err := r.Manager.GetCluster(ctx, providerProject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get provider cluster %q: %w", providerProject, err)
	}
	providerClient := providerCluster.GetClient()

	var consumer servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(ctx, req.NamespacedName, &consumer); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceConsumer: %w", err)
	}

	if !consumer.DeletionTimestamp.IsZero() {
		// The entitlement reconciler is the sole creator/deleter of consumer
		// records; nothing to do on delete here.
		return ctrl.Result{}, nil
	}

	// Without an approval decision, leave the consumer status untouched. The
	// entitlement reconciler manages the default (Active for self-service,
	// PendingApproval for gated). This reconciler only reacts to approvals.
	if consumer.Spec.Approval == nil {
		return ctrl.Result{}, nil
	}

	var (
		desiredPhase     servicesv1alpha1.ConsumerPhase
		entitlementPhase servicesv1alpha1.EntitlementPhase
		reason           string
		message          string
	)
	switch consumer.Spec.Approval.Decision {
	case servicesv1alpha1.ApprovalDecisionApproved:
		desiredPhase = servicesv1alpha1.ConsumerPhaseActive
		entitlementPhase = servicesv1alpha1.EntitlementPhaseActive
		reason = reasonConsumerApproved
		message = "Provider approved the request."
	case servicesv1alpha1.ApprovalDecisionDenied:
		desiredPhase = servicesv1alpha1.ConsumerPhaseDenied
		entitlementPhase = servicesv1alpha1.EntitlementPhaseRejected
		reason = reasonConsumerDenied
		message = "Provider denied the request."
	default:
		return ctrl.Result{}, fmt.Errorf("unexpected ApprovalDecision %q", consumer.Spec.Approval.Decision)
	}

	if err := r.updateConsumerStatus(ctx, providerClient, &consumer, desiredPhase, reason, message); err != nil {
		return ctrl.Result{}, err
	}

	// Propagate the decision back to the ServiceEntitlement in the consumer
	// project. The entitlement reconciler will re-run on the status write and
	// pick up the new phase.
	consumerProject := consumer.Spec.ConsumerProjectRef.Name
	if consumerProject == "" {
		return ctrl.Result{}, nil
	}
	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		logger.Info("consumer cluster unavailable, requeuing", "consumerProject", consumerProject, "err", err)
		return ctrl.Result{Requeue: true}, nil
	}
	consumerClient := consumerCluster.GetClient()

	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(ctx, types.NamespacedName{Name: consumer.Spec.ServiceRef.Name}, &entitlement); err != nil {
		if apierrors.IsNotFound(err) {
			// Entitlement was deleted out from under us; nothing to update.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get matching ServiceEntitlement: %w", err)
	}

	original := entitlement.Status.DeepCopy()
	entitlement.Status.Phase = entitlementPhase
	entitlement.Status.ObservedGeneration = entitlement.Generation
	if entitlementPhase == servicesv1alpha1.EntitlementPhaseActive && entitlement.Status.EntitledAt == nil {
		now := metav1.Now()
		entitlement.Status.EntitledAt = &now
	}
	apimeta.SetStatusCondition(&entitlement.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             readyStatusForEntitlementPhase(entitlementPhase),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: entitlement.Generation,
	})

	if equalEntitlementStatus(original, &entitlement.Status) {
		return ctrl.Result{}, nil
	}
	if err := consumerClient.Status().Update(ctx, &entitlement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update ServiceEntitlement status: %w", err)
	}
	return ctrl.Result{}, nil
}

func readyStatusForEntitlementPhase(phase servicesv1alpha1.EntitlementPhase) metav1.ConditionStatus {
	if phase == servicesv1alpha1.EntitlementPhaseActive {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func (r *ServiceConsumerReconciler) updateConsumerStatus(ctx context.Context, providerClient client.Client, consumer *servicesv1alpha1.ServiceConsumer, phase servicesv1alpha1.ConsumerPhase, reason, message string) error {
	original := consumer.Status.DeepCopy()
	consumer.Status.ObservedGeneration = consumer.Generation
	consumer.Status.Phase = phase
	if phase == servicesv1alpha1.ConsumerPhaseActive && consumer.Status.EntitledAt == nil {
		now := metav1.Now()
		consumer.Status.EntitledAt = &now
	}
	cond := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: consumer.Generation,
	}
	if phase == servicesv1alpha1.ConsumerPhaseActive {
		cond.Status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&consumer.Status.Conditions, cond)

	if equalConsumerStatus(original, &consumer.Status) {
		return nil
	}
	if err := providerClient.Status().Update(ctx, consumer); err != nil {
		return fmt.Errorf("failed to update ServiceConsumer status: %w", err)
	}
	return nil
}

// SetupWithManager registers the reconciler on the multicluster manager.
// WithEngageWithProviderClusters(true) — ServiceConsumers live in provider
// project virtual control planes, not the root cluster.
func (r *ServiceConsumerReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("service-consumer").
		For(&servicesv1alpha1.ServiceConsumer{}, mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(r)
}
