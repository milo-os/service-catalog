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

const monitoredResourceTypeFinalizer = "services.miloapis.com/monitored-resource-type"

const (
	// reasonMonitoredResourceTypeReady is the Ready=True reason for a
	// MonitoredResourceType that has passed all cross-reference checks.
	reasonMonitoredResourceTypeReady = "MonitoredResourceTypeReady"

	// reasonDuplicateResourceTypeName is the Ready=False reason when
	// another MonitoredResourceType already owns the same
	// spec.resourceTypeName.
	reasonDuplicateResourceTypeName = "DuplicateResourceTypeName"

	// reasonDuplicateGVK is the Ready=False reason when another
	// MonitoredResourceType already binds the same Kubernetes GVK
	// (group + kind).
	reasonDuplicateGVK = "DuplicateGVK"
)

// MonitoredResourceTypeReconciler reconciles a MonitoredResourceType
// object.
type MonitoredResourceTypeReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=monitoredresourcetypes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=monitoredresourcetypes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=monitoredresourcetypes/finalizers,verbs=update
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch

func (r *MonitoredResourceTypeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mrt servicesv1alpha1.MonitoredResourceType
	if err := r.client.Get(ctx, req.NamespacedName, &mrt); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !mrt.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &mrt)
	}

	if !controllerutil.ContainsFinalizer(&mrt, monitoredResourceTypeFinalizer) {
		controllerutil.AddFinalizer(&mrt, monitoredResourceTypeFinalizer)
		if err := r.client.Update(ctx, &mrt); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	readyCondition, err := r.desiredReadyCondition(ctx, &mrt)
	if err != nil {
		return ctrl.Result{}, err
	}
	publishedCondition := desiredPublishedCondition(mrt.Spec.Phase, mrt.Generation)

	newStatus := mrt.Status.DeepCopy()
	newStatus.ObservedGeneration = mrt.Generation
	apimeta.SetStatusCondition(&newStatus.Conditions, readyCondition)
	apimeta.SetStatusCondition(&newStatus.Conditions, publishedCondition)

	if mrt.Spec.Phase == servicesv1alpha1.PhasePublished && newStatus.PublishedAt == nil {
		now := metav1.Now()
		newStatus.PublishedAt = &now
	}

	if !monitoredResourceStatusNeedsUpdate(&mrt.Status, newStatus) {
		return ctrl.Result{}, nil
	}

	mrt.Status = *newStatus
	if err := r.client.Status().Update(ctx, &mrt); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	logger.Info("reconciled monitored resource type",
		"resourceTypeName", mrt.Spec.ResourceTypeName,
		"gvk", GVKIndexKey(mrt.Spec.GVK.Group, mrt.Spec.GVK.Kind),
		"owner", mrt.Spec.Owner.Service,
		"phase", mrt.Spec.Phase,
		"ready", readyCondition.Status,
	)

	return ctrl.Result{}, nil
}

// desiredReadyCondition builds the Ready condition. Three failure
// modes are surfaced: a duplicate resourceTypeName, a duplicate
// group+kind binding, and an owning Service that is either missing or
// not yet Published.
func (r *MonitoredResourceTypeReconciler) desiredReadyCondition(
	ctx context.Context,
	mrt *servicesv1alpha1.MonitoredResourceType,
) (metav1.Condition, error) {
	var nameDupes servicesv1alpha1.MonitoredResourceTypeList
	if err := r.client.List(ctx, &nameDupes,
		client.MatchingFields{MonitoredResourceTypeResourceTypeNameField: mrt.Spec.ResourceTypeName},
	); err != nil {
		return metav1.Condition{}, fmt.Errorf(
			"failed to list duplicates for %q: %w", mrt.Spec.ResourceTypeName, err)
	}
	for i := range nameDupes.Items {
		other := &nameDupes.Items[i]
		if other.UID == mrt.UID {
			continue
		}
		return metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: mrt.Generation,
			Reason:             reasonDuplicateResourceTypeName,
			Message: fmt.Sprintf("resourceTypeName %q is also defined by %q; resolve the collision before use",
				mrt.Spec.ResourceTypeName, other.Name),
		}, nil
	}

	var gvkDupes servicesv1alpha1.MonitoredResourceTypeList
	gvkKey := GVKIndexKey(mrt.Spec.GVK.Group, mrt.Spec.GVK.Kind)
	if err := r.client.List(ctx, &gvkDupes,
		client.MatchingFields{MonitoredResourceTypeGVKField: gvkKey},
	); err != nil {
		return metav1.Condition{}, fmt.Errorf("failed to list GVK duplicates for %q: %w", gvkKey, err)
	}
	for i := range gvkDupes.Items {
		other := &gvkDupes.Items[i]
		if other.UID == mrt.UID {
			continue
		}
		return metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: mrt.Generation,
			Reason:             reasonDuplicateGVK,
			Message: fmt.Sprintf("Kubernetes Kind %q is also bound by %q; only one MonitoredResourceType may own a Kind",
				gvkKey, other.Name),
		}, nil
	}

	var services servicesv1alpha1.ServiceList
	if err := r.client.List(ctx, &services,
		client.MatchingFields{ServiceServiceNameField: mrt.Spec.Owner.Service},
	); err != nil {
		return metav1.Condition{}, fmt.Errorf(
			"failed to list owning services for %q: %w", mrt.Spec.Owner.Service, err)
	}
	if !anyServicePublished(services.Items) {
		return metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: mrt.Generation,
			Reason:             reasonOwnerNotPublished,
			Message: fmt.Sprintf("owning service %q is not in the Published phase; monitored resource type is not referenceable",
				mrt.Spec.Owner.Service),
		}, nil
	}

	return metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: mrt.Generation,
		Reason:             reasonMonitoredResourceTypeReady,
		Message:            "Monitored resource type is active and available to downstream systems.",
	}, nil
}

func monitoredResourceStatusNeedsUpdate(
	current, desired *servicesv1alpha1.MonitoredResourceTypeStatus,
) bool {
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

func (r *MonitoredResourceTypeReconciler) reconcileDelete(
	ctx context.Context,
	mrt *servicesv1alpha1.MonitoredResourceType,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(mrt, monitoredResourceTypeFinalizer) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(mrt, monitoredResourceTypeFinalizer)
	if err := r.client.Update(ctx, mrt); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	logger.Info("finalized monitored resource type", "resourceTypeName", mrt.Spec.ResourceTypeName)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. Changes
// to the owning Service trigger a requeue of every
// MonitoredResourceType that names it.
func (r *MonitoredResourceTypeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()

	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoredresourcetype").
		For(&servicesv1alpha1.MonitoredResourceType{}).
		Watches(&servicesv1alpha1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.monitoredResourceTypesForService),
		).
		Complete(r)
}

func (r *MonitoredResourceTypeReconciler) monitoredResourceTypesForService(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	svc, ok := obj.(*servicesv1alpha1.Service)
	if !ok {
		return nil
	}

	var list servicesv1alpha1.MonitoredResourceTypeList
	if err := r.client.List(ctx, &list,
		client.MatchingFields{MonitoredResourceTypeOwnerServiceField: svc.Spec.ServiceName},
	); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: list.Items[i].Name},
		})
	}
	return requests
}
