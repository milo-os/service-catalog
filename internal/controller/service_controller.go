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

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// serviceFinalizer is placed on every Service so that future
// governance/reference checks (MeterDefinition, MonitoredResourceType,
// quota, marketplace) can block deletion while references exist. For
// v0 the finalizer is removed immediately; the slot is reserved so
// reference checks can be added later without an API change.
const serviceFinalizer = "services.miloapis.com/service"

const (
	// ConditionTypeReady is the condition type used across the
	// services group to surface overall resource readiness.
	ConditionTypeReady = "Ready"

	// ConditionTypePublished mirrors spec.phase: Status=True when
	// the resource is in one of the post-Draft phases (Published,
	// Deprecated, Retired), Status=False while the resource is still
	// Draft.
	ConditionTypePublished = "Published"

	// reasonServiceReady is the Ready=True reason for a Service that
	// the controller has reconciled to a steady state.
	reasonServiceReady = "ServiceReady"
)

// ServiceReconciler reconciles a Service object.
type ServiceReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services/finalizers,verbs=update

func (r *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var svc servicesv1alpha1.Service
	if err := r.client.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !svc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &svc)
	}

	if !controllerutil.ContainsFinalizer(&svc, serviceFinalizer) {
		controllerutil.AddFinalizer(&svc, serviceFinalizer)
		if err := r.client.Update(ctx, &svc); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Phase is now declared on spec; the controller never mutates it.
	// Mirror it into status as conditions, and stamp PublishedAt the
	// first time we observe spec.phase == Published.
	newStatus := svc.Status.DeepCopy()
	newStatus.ObservedGeneration = svc.Generation

	if svc.Spec.Phase == servicesv1alpha1.PhasePublished && newStatus.PublishedAt == nil {
		now := metav1.Now()
		newStatus.PublishedAt = &now
	}

	readyCondition := r.desiredReadyCondition(&svc)
	apimeta.SetStatusCondition(&newStatus.Conditions, readyCondition)

	publishedCondition := desiredPublishedCondition(svc.Spec.Phase, svc.Generation)
	apimeta.SetStatusCondition(&newStatus.Conditions, publishedCondition)

	if !serviceStatusNeedsUpdate(&svc.Status, newStatus) {
		return ctrl.Result{}, nil
	}

	svc.Status = *newStatus
	if err := r.client.Status().Update(ctx, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update service status: %w", err)
	}

	logger.Info("reconciled service",
		"serviceName", svc.Spec.ServiceName,
		"phase", svc.Spec.Phase,
		"ready", readyCondition.Status,
	)

	return ctrl.Result{}, nil
}

// desiredReadyCondition returns the Ready condition for a Service.
// Ready is not gated on phase: a Draft Service whose invariants hold is
// still Ready=True. (The invariants the webhook enforces -- slug shape,
// serviceName shape, owner reference -- are checked at admission; this
// controller has no additional invariants to verify today, so the
// default is Ready=True.)
func (r *ServiceReconciler) desiredReadyCondition(
	svc *servicesv1alpha1.Service,
) metav1.Condition {
	return metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: svc.Generation,
		Reason:             reasonServiceReady,
		Message:            "Service is reconciled.",
	}
}

// desiredPublishedCondition builds the Published condition, mirroring
// spec.phase. Status=True when the Service is in one of the post-Draft
// phases; Status=False while still Draft.
func desiredPublishedCondition(phase servicesv1alpha1.Phase, generation int64) metav1.Condition {
	// Default to Draft semantics when phase is empty (should not occur
	// in practice: the spec default is Draft).
	effective := phase
	if effective == "" {
		effective = servicesv1alpha1.PhaseDraft
	}

	cond := metav1.Condition{
		Type:               ConditionTypePublished,
		ObservedGeneration: generation,
		Reason:             "PhaseIs" + string(effective),
	}

	switch effective {
	case servicesv1alpha1.PhasePublished,
		servicesv1alpha1.PhaseDeprecated,
		servicesv1alpha1.PhaseRetired:
		cond.Status = metav1.ConditionTrue
		cond.Message = fmt.Sprintf("spec.phase is %s.", effective)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Message = "spec.phase is Draft; resource is not published."
	}
	return cond
}

// serviceStatusNeedsUpdate returns true when desired status diverges
// from the observed status enough to justify a status write.
func serviceStatusNeedsUpdate(current, desired *servicesv1alpha1.ServiceStatus) bool {
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

// conditionsEqual returns true when the condition of the given type is
// present and equal (status/reason/message/observedGeneration) in both
// lists.
func conditionsEqual(a, b []metav1.Condition, t string) bool {
	ca := apimeta.FindStatusCondition(a, t)
	cb := apimeta.FindStatusCondition(b, t)
	if ca == nil || cb == nil {
		return ca == cb
	}
	return ca.Status == cb.Status &&
		ca.Reason == cb.Reason &&
		ca.Message == cb.Message &&
		ca.ObservedGeneration == cb.ObservedGeneration
}

func (r *ServiceReconciler) reconcileDelete(
	ctx context.Context,
	svc *servicesv1alpha1.Service,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(svc, serviceFinalizer) {
		return ctrl.Result{}, nil
	}

	// TODO: block deletion while MeterDefinitions or
	// MonitoredResourceTypes still reference spec.serviceName. The
	// indexers are in place; the check is deferred to v1 so the v0
	// lifecycle stays self-service.
	controllerutil.RemoveFinalizer(svc, serviceFinalizer)
	if err := r.client.Update(ctx, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	logger.Info("finalized service", "serviceName", svc.Spec.ServiceName)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()

	return ctrl.NewControllerManagedBy(mgr).
		Named("service").
		For(&servicesv1alpha1.Service{}).
		Complete(r)
}
