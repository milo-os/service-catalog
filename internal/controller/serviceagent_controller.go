// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// serviceAgentFinalizer is placed on every ServiceAgent so that future
// reference checks (ServiceAgentConfiguration, AgentBinding) can block
// deletion while references exist. For v0 the finalizer is removed
// immediately; the slot is reserved so reference checks can be added
// later without an API change — mirroring serviceFinalizer.
const serviceAgentFinalizer = "services.miloapis.com/service-agent"

// reasonServiceAgentReady is the Ready=True reason for a ServiceAgent
// that the controller has reconciled to a steady state.
const reasonServiceAgentReady = "ServiceAgentReady"

// ServiceAgentReconciler reconciles a ServiceAgent object. Phase is
// declared on spec; the controller mirrors it into status as the
// Published condition and stamps PublishedAt the first time it observes
// spec.phase == Published — the same contract ServiceReconciler
// implements for Service.
type ServiceAgentReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents/finalizers,verbs=update

func (r *ServiceAgentReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var agent servicesv1alpha1.ServiceAgent
	if err := r.client.Get(ctx, req.NamespacedName, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !agent.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &agent)
	}

	if !controllerutil.ContainsFinalizer(&agent, serviceAgentFinalizer) {
		controllerutil.AddFinalizer(&agent, serviceAgentFinalizer)
		if err := r.client.Update(ctx, &agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	newStatus := agent.Status.DeepCopy()
	newStatus.ObservedGeneration = agent.Generation

	if agent.Spec.Phase == servicesv1alpha1.PhasePublished && newStatus.PublishedAt == nil {
		now := metav1.Now()
		newStatus.PublishedAt = &now
	}

	// Ready is not gated on phase: the invariants the webhook enforces
	// (serviceRef resolution, phase transitions) are checked at
	// admission; this controller has no additional invariants to verify
	// today — mirroring ServiceReconciler.
	readyCondition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: agent.Generation,
		Reason:             reasonServiceAgentReady,
		Message:            "ServiceAgent is reconciled.",
	}
	apimeta.SetStatusCondition(&newStatus.Conditions, readyCondition)

	publishedCondition := desiredPublishedCondition(agent.Spec.Phase, agent.Generation)
	apimeta.SetStatusCondition(&newStatus.Conditions, publishedCondition)

	if !catalogStatusNeedsUpdate(&agent.Status.CatalogStatus, &newStatus.CatalogStatus) {
		return ctrl.Result{}, nil
	}

	agent.Status = *newStatus
	if err := r.client.Status().Update(ctx, &agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update serviceagent status: %w", err)
	}

	logger.Info("reconciled serviceagent",
		"name", agent.Name,
		"service", agent.Spec.ServiceRef.Name,
		"phase", agent.Spec.Phase,
	)
	return ctrl.Result{}, nil
}

// catalogStatusNeedsUpdate returns true when the desired shared catalog
// status diverges from the observed one enough to justify a status
// write. Mirrors serviceStatusNeedsUpdate for CatalogStatus embedders.
func catalogStatusNeedsUpdate(current, desired *servicesv1alpha1.CatalogStatus) bool {
	if current.ObservedGeneration != desired.ObservedGeneration {
		return true
	}
	if (current.PublishedAt == nil) != (desired.PublishedAt == nil) {
		return true
	}
	if !conditionsEqual(current.Conditions, desired.Conditions, ConditionTypeReady) {
		return true
	}
	if !conditionsEqual(current.Conditions, desired.Conditions, ConditionTypePublished) {
		return true
	}
	return false
}

func (r *ServiceAgentReconciler) reconcileDelete(
	ctx context.Context,
	agent *servicesv1alpha1.ServiceAgent,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(agent, serviceAgentFinalizer) {
		return ctrl.Result{}, nil
	}

	// TODO: block deletion while ServiceAgentConfigurations still
	// reference this agent, mirroring the deferred Service reference
	// check.
	controllerutil.RemoveFinalizer(agent, serviceAgentFinalizer)
	if err := r.client.Update(ctx, agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	logger.Info("finalized serviceagent", "name", agent.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()

	return ctrl.NewControllerManagedBy(mgr).
		Named("service-agent").
		For(&servicesv1alpha1.ServiceAgent{}).
		Complete(r)
}
