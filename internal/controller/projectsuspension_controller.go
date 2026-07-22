// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// ConditionTypeSuspended is the condition type written to both
	// ServiceConsumer and ServiceEntitlement status to mirror their owning
	// Project's own Suspended condition.
	ConditionTypeSuspended = servicesv1alpha1.ConditionTypeSuspended

	reasonProjectSuspended = servicesv1alpha1.ReasonProjectSuspended
	reasonProjectActive    = servicesv1alpha1.ReasonProjectActive

	// suspendedConditionFieldManager is the field manager both writers of the
	// Suspended condition — this reconciler and ServiceEntitlementReconciler
	// — pass to client.FieldOwner on their merge patch, mirroring the
	// pattern Milo's own ProjectSuspensionPropagatorController uses for the
	// Project's Suspended condition.
	suspendedConditionFieldManager = "services-suspended-condition"
)

// projectSuspension captures a Project's own Suspended condition — status
// and human-readable reason — for propagation onto ServiceConsumer and
// ServiceEntitlement. Carrying the message through means a provider or
// consumer sees the same specific reason (e.g. "Project is suspended due to
// active suspensions: nonpayment") Milo wrote on the Project, not a generic
// stand-in.
type projectSuspension struct {
	Suspended bool
	Message   string
}

// suspensionFromProject reads a Project's own Suspended condition.
func suspensionFromProject(project *resourcemanagerv1alpha1.Project) projectSuspension {
	cond := apimeta.FindStatusCondition(project.Status.Conditions, resourcemanagerv1alpha1.ProjectSuspended)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return projectSuspension{}
	}
	return projectSuspension{Suspended: true, Message: cond.Message}
}

// suspendedCondition builds the Suspended condition mirroring a Project's own
// Suspended condition shape (Status/Reason/Message/LastTransitionTime), so
// providers and consumers can see suspension state — and why — at a glance
// without cross-referencing the Project directly.
func suspendedCondition(suspension projectSuspension, generation int64) metav1.Condition {
	cond := metav1.Condition{
		Type:               ConditionTypeSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             reasonProjectActive,
		Message:            "The owning project is active.",
		ObservedGeneration: generation,
	}
	if suspension.Suspended {
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonProjectSuspended
		cond.Message = "The owning project is suspended; access is paused until it's reinstated."
		if suspension.Message != "" {
			cond.Message = suspension.Message
		}
	}
	return cond
}

// patchConsumerSuspendedCondition sets the Suspended condition on a
// ServiceConsumer's status and, if that changed anything, patches it via a
// merge patch computed from the before/after diff — the same
// Status().Patch(ctx, obj, client.MergeFrom(before), client.FieldOwner(...))
// pattern Milo's own ProjectSuspensionPropagatorController uses for the
// Project's own Suspended condition.
func patchConsumerSuspendedCondition(ctx context.Context, providerClient client.Client, consumer *servicesv1alpha1.ServiceConsumer, suspension projectSuspension) error {
	before := consumer.DeepCopy()
	apimeta.SetStatusCondition(&consumer.Status.Conditions, suspendedCondition(suspension, consumer.Generation))
	if conditionsEqual(before.Status.Conditions, consumer.Status.Conditions, ConditionTypeSuspended) {
		return nil
	}
	if err := providerClient.Status().Patch(ctx, consumer, client.MergeFrom(before), client.FieldOwner(suspendedConditionFieldManager)); err != nil {
		return fmt.Errorf("failed to patch ServiceConsumer status: %w", err)
	}
	return nil
}

// patchEntitlementSuspendedCondition is patchConsumerSuspendedCondition's
// counterpart for the ServiceEntitlement side of the propagation.
func patchEntitlementSuspendedCondition(ctx context.Context, consumerClient client.Client, entitlement *servicesv1alpha1.ServiceEntitlement, suspension projectSuspension) error {
	before := entitlement.DeepCopy()
	apimeta.SetStatusCondition(&entitlement.Status.Conditions, suspendedCondition(suspension, entitlement.Generation))
	if conditionsEqual(before.Status.Conditions, entitlement.Status.Conditions, ConditionTypeSuspended) {
		return nil
	}
	if err := consumerClient.Status().Patch(ctx, entitlement, client.MergeFrom(before), client.FieldOwner(suspendedConditionFieldManager)); err != nil {
		return fmt.Errorf("failed to patch ServiceEntitlement status: %w", err)
	}
	return nil
}

// ProjectSuspensionPropagationReconciler propagates the Project's Suspension state
// to all ServiceConsumers associated with that project.
type ProjectSuspensionPropagationReconciler struct {
	client  client.Client
	Scheme  *runtime.Scheme
	Manager mcmanager.Manager
}

// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/status,verbs=get;update;patch

func (r *ProjectSuspensionPropagationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var project resourcemanagerv1alpha1.Project
	if err := r.client.Get(ctx, req.NamespacedName, &project); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Project: %w", err)
	}

	// 1. Determine if project is suspended.
	suspension := suspensionFromProject(&project)

	// 2. Fetch the cluster client for the project.
	cluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(project.Name))
	if err != nil {
		// Project cluster might not be engaged yet; skip or requeue
		logger.V(1).Info("project cluster not engaged yet, skipping propagation", "project", project.Name)
		return ctrl.Result{}, nil
	}
	consumerClient := cluster.GetClient()

	// 3. List all ServiceEntitlement resources in the project cluster.
	var entitlements servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &entitlements); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceEntitlements: %w", err)
	}

	for i := range entitlements.Items {
		ent := &entitlements.Items[i]
		// A ServiceConsumer is upserted for every entitlement phase (Active,
		// PendingApproval, Rejected) — only skip entitlements that never got
		// far enough to have one.
		if ent.Status.Phase == "" {
			continue
		}

		// Push the same signal onto the entitlement's own status, so the
		// consumer project admin can see it's not active because their
		// project is suspended without needing visibility into the
		// provider's ServiceConsumer.
		if err := patchEntitlementSuspendedCondition(ctx, consumerClient, ent, suspension); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to apply Suspended condition on ServiceEntitlement %q: %w", ent.Name, err)
		}

		// 4. Resolve the Service to find the provider project. Use the same
		// canonical-name-first lookup as the entitlement controller, since
		// spec.serviceRef.name may hold either the canonical service name or
		// the Kubernetes object name.
		svc, err := resolveService(ctx, r.client, ent.Spec.ServiceRef.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", ent.Spec.ServiceRef.Name, err)
		}

		if svc.Spec.Phase == servicesv1alpha1.PhaseDraft {
			continue
		}

		providerProject := svc.Spec.Owner.ProducerProjectRef.Name
		providerCluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(providerProject))
		if err != nil {
			logger.Info("provider cluster not engaged, skipping consumer updates", "providerProject", providerProject)
			continue
		}
		providerClient := providerCluster.GetClient()

		// 5. Update the ServiceConsumer status.
		consumerName := serviceConsumerName(svc.Spec.ServiceName, project.Name)
		var consumer servicesv1alpha1.ServiceConsumer
		if err := providerClient.Get(ctx, client.ObjectKey{Name: consumerName}, &consumer); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get ServiceConsumer %q: %w", consumerName, err)
		}

		if suspension.Suspended != apimeta.IsStatusConditionTrue(consumer.Status.Conditions, ConditionTypeSuspended) {
			if err := patchConsumerSuspendedCondition(ctx, providerClient, &consumer, suspension); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to apply Suspended condition on ServiceConsumer %q: %w", consumerName, err)
			}
			logger.Info("propagated project suspension state to ServiceConsumer",
				"project", project.Name,
				"serviceConsumer", consumerName,
				"suspended", suspension.Suspended,
			)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler on the root manager.
func (r *ProjectSuspensionPropagationReconciler) SetupWithManager(mgr ctrl.Manager, mcMgr mcmanager.Manager) error {
	r.client = mgr.GetClient()
	r.Manager = mcMgr

	return ctrl.NewControllerManagedBy(mgr).
		For(&resourcemanagerv1alpha1.Project{}).
		Named("project-suspension-propagation").
		Complete(r)
}
