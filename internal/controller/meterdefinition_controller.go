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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const meterDefinitionFinalizer = "services.miloapis.com/meter-definition"

const (
	// reasonMeterDefinitionReady is the Ready=True reason for a
	// MeterDefinition that has passed all cross-reference checks.
	reasonMeterDefinitionReady = "MeterDefinitionReady"

	// reasonDuplicateMeterName is the Ready=False reason when another
	// MeterDefinition already owns the same spec.meterName. The webhook
	// catches the common case; this condition surfaces the race between
	// two concurrent admissions.
	reasonDuplicateMeterName = "DuplicateMeterName"

	// reasonOwnerNotPublished is the Ready=False reason when the owning
	// Service is either missing or not yet in the Published phase. The
	// meter stays visible but is not referenceable downstream.
	reasonOwnerNotPublished = "OwnerNotPublished"
)

// MeterDefinitionReconciler reconciles a MeterDefinition object.
type MeterDefinitionReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=meterdefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=meterdefinitions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=meterdefinitions/finalizers,verbs=update
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch

func (r *MeterDefinitionReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var md servicesv1alpha1.MeterDefinition
	if err := r.client.Get(ctx, req.NamespacedName, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !md.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &md)
	}

	if !controllerutil.ContainsFinalizer(&md, meterDefinitionFinalizer) {
		controllerutil.AddFinalizer(&md, meterDefinitionFinalizer)
		if err := r.client.Update(ctx, &md); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	readyCondition, err := r.desiredReadyCondition(ctx, &md)
	if err != nil {
		return ctrl.Result{}, err
	}
	publishedCondition := desiredPublishedCondition(md.Spec.Phase, md.Generation)

	newStatus := md.Status.DeepCopy()
	newStatus.ObservedGeneration = md.Generation
	apimeta.SetStatusCondition(&newStatus.Conditions, readyCondition)
	apimeta.SetStatusCondition(&newStatus.Conditions, publishedCondition)

	if md.Spec.Phase == servicesv1alpha1.PhasePublished && newStatus.PublishedAt == nil {
		now := metav1.Now()
		newStatus.PublishedAt = &now
	}

	if !meterStatusNeedsUpdate(&md.Status, newStatus) {
		return ctrl.Result{}, nil
	}

	md.Status = *newStatus
	if err := r.client.Status().Update(ctx, &md); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	logger.Info("reconciled meter definition",
		"meterName", md.Spec.MeterName,
		"owner", md.Spec.Owner.Service,
		"phase", md.Spec.Phase,
		"ready", readyCondition.Status,
	)

	return ctrl.Result{}, nil
}

// desiredReadyCondition builds the Ready condition. Two failure modes
// are surfaced: a duplicate meterName (race past the webhook) and an
// owning Service that is either missing or not yet Published.
func (r *MeterDefinitionReconciler) desiredReadyCondition(
	ctx context.Context,
	md *servicesv1alpha1.MeterDefinition,
) (metav1.Condition, error) {
	var dupes servicesv1alpha1.MeterDefinitionList
	if err := r.client.List(ctx, &dupes,
		client.MatchingFields{MeterDefinitionMeterNameField: md.Spec.MeterName},
	); err != nil {
		return metav1.Condition{}, fmt.Errorf("failed to list duplicates for %q: %w", md.Spec.MeterName, err)
	}
	for i := range dupes.Items {
		other := &dupes.Items[i]
		if other.UID == md.UID {
			continue
		}
		return metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: md.Generation,
			Reason:             reasonDuplicateMeterName,
			Message: fmt.Sprintf("meterName %q is also defined by %q; resolve the collision before use",
				md.Spec.MeterName, other.Name),
		}, nil
	}

	var services servicesv1alpha1.ServiceList
	if err := r.client.List(ctx, &services,
		client.MatchingFields{ServiceServiceNameField: md.Spec.Owner.Service},
	); err != nil {
		return metav1.Condition{}, fmt.Errorf("failed to list owning services for %q: %w", md.Spec.Owner.Service, err)
	}
	if !anyServicePublished(services.Items) {
		return metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: md.Generation,
			Reason:             reasonOwnerNotPublished,
			Message: fmt.Sprintf("owning service %q is not in the Published phase; meter is not referenceable",
				md.Spec.Owner.Service),
		}, nil
	}

	return metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: md.Generation,
		Reason:             reasonMeterDefinitionReady,
		Message:            "Meter definition is active and available to downstream systems.",
	}, nil
}

func anyServicePublished(list []servicesv1alpha1.Service) bool {
	for i := range list {
		if list[i].Spec.Phase == servicesv1alpha1.PhasePublished {
			return true
		}
	}
	return false
}

func meterStatusNeedsUpdate(current, desired *servicesv1alpha1.MeterDefinitionStatus) bool {
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

func (r *MeterDefinitionReconciler) reconcileDelete(
	ctx context.Context,
	md *servicesv1alpha1.MeterDefinition,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(md, meterDefinitionFinalizer) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(md, meterDefinitionFinalizer)
	if err := r.client.Update(ctx, md); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	logger.Info("finalized meter definition", "meterName", md.Spec.MeterName)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. Changes to
// the owning Service trigger a requeue of every MeterDefinition that
// names it so Ready flips promptly when the Service is published.
func (r *MeterDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()

	return ctrl.NewControllerManagedBy(mgr).
		Named("meterdefinition").
		For(&servicesv1alpha1.MeterDefinition{}).
		Watches(&servicesv1alpha1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.meterDefinitionsForService),
		).
		Complete(r)
}

func (r *MeterDefinitionReconciler) meterDefinitionsForService(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	svc, ok := obj.(*servicesv1alpha1.Service)
	if !ok {
		return nil
	}

	var mdList servicesv1alpha1.MeterDefinitionList
	if err := r.client.List(ctx, &mdList,
		client.MatchingFields{MeterDefinitionOwnerServiceField: svc.Spec.ServiceName},
	); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(mdList.Items))
	for i := range mdList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: mdList.Items[i].Name},
		})
	}
	return requests
}
