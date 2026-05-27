// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// ConditionTypeAvailable is the gate-3 condition the
	// ServiceAvailabilityReconciler owns: True when the referenced Service
	// is Published and the referenced Location exists and is Ready. The
	// LocationBindingReconciler reads it as the third gate in the location
	// three-gate model.
	ConditionTypeAvailable = "Available"

	// reasonServiceOperational is the Available=True reason: every gate is
	// open and the service is operational at the location.
	reasonServiceOperational = "ServiceOperational"

	// reasonLocationNotFound is the Available=False reason when
	// spec.locationRef does not resolve to a Location.
	reasonLocationNotFound = "LocationNotFound"

	// reasonLocationNotReady is the Available=False reason when the
	// referenced Location exists but its Ready condition is not True.
	reasonLocationNotReady = "LocationNotReady"
	// reasonServiceNotPublished ("ServiceNotPublished") is shared with the
	// ServiceEntitlement reconciler; it is declared there.
)

// locationGVK is the GroupVersionKind the reconciler reads to evaluate gate
// 2 (Location.status.conditions[Ready]). Location is owned by the
// network-services operator and lives in networking.datumapis.com; reading
// it as an unstructured object keeps ServiceAvailability free of a
// compile-time dependency on that Go module, matching the deliberate choice
// in the LocationRef type definition.
var locationGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha1",
	Kind:    "Location",
}

// ServiceAvailabilityReconciler reconciles a ServiceAvailability object. It
// owns the Available condition (gate 3): True only when spec.serviceRef
// resolves to a Published Service and spec.locationRef resolves to a Ready
// Location.
type ServiceAvailabilityReconciler struct {
	client client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceavailabilities/finalizers,verbs=update
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=locations,verbs=get;list;watch

func (r *ServiceAvailabilityReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sa servicesv1alpha1.ServiceAvailability
	if err := r.client.Get(ctx, req.NamespacedName, &sa); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion is left to garbage collection: the reconciler holds no
	// finalizer because nothing downstream is gated on this object by the
	// reconciler itself.
	if !sa.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// A transient read error (anything other than NotFound) on the
	// referenced Service or Location must requeue rather than flip the
	// condition to False on a blip.
	available, err := r.desiredAvailableCondition(ctx, &sa)
	if err != nil {
		return ctrl.Result{}, err
	}

	newStatus := sa.Status.DeepCopy()
	newStatus.ObservedGeneration = sa.Generation
	apimeta.SetStatusCondition(&newStatus.Conditions, available)

	if !availabilityStatusNeedsUpdate(&sa.Status, newStatus) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(sa.DeepCopy())
	sa.Status = *newStatus
	if err := r.client.Status().Patch(ctx, &sa, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch ServiceAvailability status: %w", err)
	}

	logger.Info("reconciled service availability",
		"service", sa.Spec.ServiceRef.Name,
		"location", sa.Spec.LocationRef.Name,
		"available", available.Status,
		"reason", available.Reason,
	)
	return ctrl.Result{}, nil
}

// desiredAvailableCondition evaluates the three gates and returns the
// Available condition. It returns a non-nil error only for transient read
// failures, which the caller turns into a requeue; a resolved "gate closed"
// outcome is encoded as Available=False, never as an error.
func (r *ServiceAvailabilityReconciler) desiredAvailableCondition(
	ctx context.Context,
	sa *servicesv1alpha1.ServiceAvailability,
) (metav1.Condition, error) {
	cond := metav1.Condition{
		Type:               ConditionTypeAvailable,
		ObservedGeneration: sa.Generation,
	}

	// Gate 1: the referenced Service must exist and be Published.
	var svc servicesv1alpha1.Service
	if err := r.client.Get(ctx, types.NamespacedName{Name: sa.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return deny(cond, reasonServiceNotPublished,
				fmt.Sprintf("no Service with metadata.name %q exists", sa.Spec.ServiceRef.Name)), nil
		}
		return cond, fmt.Errorf("failed to load referenced Service: %w", err)
	}
	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		return deny(cond, reasonServiceNotPublished,
			fmt.Sprintf("Service %q is in phase %q; only a Published Service is available", svc.Name, svc.Spec.Phase)), nil
	}

	// Gate 2: the referenced Location must exist and be Ready.
	loc := &unstructured.Unstructured{}
	loc.SetGroupVersionKind(locationGVK)
	locKey := types.NamespacedName{
		Name:      sa.Spec.LocationRef.Name,
		Namespace: sa.Spec.LocationRef.Namespace,
	}
	if err := r.client.Get(ctx, locKey, loc); err != nil {
		if apierrors.IsNotFound(err) {
			return deny(cond, reasonLocationNotFound,
				fmt.Sprintf("no Location %q in namespace %q exists", locKey.Name, locKey.Namespace)), nil
		}
		return cond, fmt.Errorf("failed to load referenced Location: %w", err)
	}
	if !locationReady(loc) {
		return deny(cond, reasonLocationNotReady,
			fmt.Sprintf("Location %q is not Ready", locKey.Name)), nil
	}

	// All gates open.
	cond.Status = metav1.ConditionTrue
	cond.Reason = reasonServiceOperational
	cond.Message = "Service is deployed and the location is Ready."
	return cond, nil
}

// deny fills in an Available=False condition with the given reason/message.
func deny(cond metav1.Condition, reason, message string) metav1.Condition {
	cond.Status = metav1.ConditionFalse
	cond.Reason = reason
	cond.Message = message
	return cond
}

// locationReady reports whether the Location's status carries a Ready
// condition with status "True". It reads the unstructured status.conditions
// list defensively: a missing or malformed conditions entry is treated as
// not-Ready rather than an error.
func locationReady(loc *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(loc.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(cond, "type")
		if condType != "Ready" {
			continue
		}
		status, _, _ := unstructured.NestedString(cond, "status")
		return status == string(metav1.ConditionTrue)
	}
	return false
}

// availabilityStatusNeedsUpdate returns true when the desired status
// diverges from the observed status enough to justify a status write.
func availabilityStatusNeedsUpdate(current, desired *servicesv1alpha1.ServiceAvailabilityStatus) bool {
	if current.ObservedGeneration != desired.ObservedGeneration {
		return true
	}
	return !conditionsEqual(current.Conditions, desired.Conditions, ConditionTypeAvailable)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceAvailabilityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()

	return ctrl.NewControllerManagedBy(mgr).
		Named("service-availability").
		For(&servicesv1alpha1.ServiceAvailability{}).
		Complete(r)
}
