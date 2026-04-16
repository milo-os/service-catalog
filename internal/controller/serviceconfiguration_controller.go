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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const serviceConfigurationFinalizer = "services.miloapis.com/serviceconfiguration-protection"

const (
	// ConditionTypeBillingFanOutHealthy surfaces whether the billing
	// fan-out is up to date with the current ServiceConfiguration spec.
	ConditionTypeBillingFanOutHealthy = "BillingFanOutHealthy"

	reasonServiceConfigurationReady = "ServiceConfigurationReady"
	reasonServiceRefNotFound        = "ServiceRefNotFound"
	reasonBillingFanOutFailed       = "BillingFanOutFailed"
	reasonBillingFanOutHealthy      = "BillingFanOutHealthy"
	reasonBillingFanOutSkipped      = "BillingFanOutSkipped"
)

// ServiceConfigurationReconciler reconciles a ServiceConfiguration
// object. It owns the billing fan-out: changes to the document
// materialize as billing.miloapis.com/MeterDefinition and
// billing.miloapis.com/MonitoredResourceType objects via server-side
// apply, and previously-managed objects no longer in the desired set are
// deleted.
type ServiceConfigurationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	BillingFanOut *BillingFanOut
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations/finalizers,verbs=update

func (r *ServiceConfigurationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sc servicesv1alpha1.ServiceConfiguration
	if err := r.Get(ctx, req.NamespacedName, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetch ServiceConfiguration: %w", err)
	}

	if !sc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &sc)
	}

	if !controllerutil.ContainsFinalizer(&sc, serviceConfigurationFinalizer) {
		controllerutil.AddFinalizer(&sc, serviceConfigurationFinalizer)
		if err := r.Update(ctx, &sc); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	var svc servicesv1alpha1.Service
	if err := r.Get(ctx, client.ObjectKey{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("referenced Service %q not found", sc.Spec.ServiceRef.Name)
			return ctrl.Result{}, r.writeStatusConditions(ctx, &sc,
				metav1.Condition{
					Type:               ConditionTypeReady,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            msg,
				},
				metav1.Condition{
					Type:               ConditionTypeBillingFanOutHealthy,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            msg + "; cannot fan out",
				},
			)
		}
		return ctrl.Result{}, fmt.Errorf("fetch referenced Service %q: %w", sc.Spec.ServiceRef.Name, err)
	}

	fanOutCondition := metav1.Condition{
		Type:               ConditionTypeBillingFanOutHealthy,
		ObservedGeneration: sc.Generation,
	}
	readyCondition := metav1.Condition{
		Type:               ConditionTypeReady,
		ObservedGeneration: sc.Generation,
	}

	var fanOutErr error
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		fanOutCondition.Status = metav1.ConditionTrue
		fanOutCondition.Reason = reasonBillingFanOutSkipped
		fanOutCondition.Message = "ServiceConfiguration is Draft; fan-out skipped."
	} else {
		fanOutErr = r.BillingFanOut.Reconcile(ctx, &sc)
		if fanOutErr != nil {
			fanOutCondition.Status = metav1.ConditionFalse
			fanOutCondition.Reason = reasonBillingFanOutFailed
			fanOutCondition.Message = fmt.Sprintf("billing fan-out failed: %v", fanOutErr)
		} else {
			fanOutCondition.Status = metav1.ConditionTrue
			fanOutCondition.Reason = reasonBillingFanOutHealthy
			fanOutCondition.Message = "Billing fan-out reconciled successfully."
		}
	}

	if fanOutErr != nil {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonBillingFanOutFailed
		readyCondition.Message = fanOutCondition.Message
	} else {
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = reasonServiceConfigurationReady
		readyCondition.Message = "ServiceConfiguration is reconciled."
	}

	if err := r.writeStatusConditions(ctx, &sc, readyCondition, fanOutCondition); err != nil {
		return ctrl.Result{}, err
	}

	if fanOutErr != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceConfiguration: %w", fanOutErr)
	}

	logger.Info("reconciled serviceconfiguration",
		"name", sc.Name,
		"service", svc.Spec.ServiceName,
		"phase", sc.Spec.Phase,
	)
	return ctrl.Result{}, nil
}

func (r *ServiceConfigurationReconciler) reconcileDelete(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(sc, serviceConfigurationFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.BillingFanOut.Cleanup(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup billing objects: %w", err)
	}
	controllerutil.RemoveFinalizer(sc, serviceConfigurationFinalizer)
	if err := r.Update(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	logger.Info("finalized serviceconfiguration", "name", sc.Name)
	return ctrl.Result{}, nil
}

func (r *ServiceConfigurationReconciler) writeStatusConditions(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	conds ...metav1.Condition,
) error {
	newStatus := sc.Status.DeepCopy()
	newStatus.ObservedGeneration = sc.Generation
	for _, c := range conds {
		apimeta.SetStatusCondition(&newStatus.Conditions, c)
	}
	apimeta.SetStatusCondition(&newStatus.Conditions, desiredPublishedCondition(sc.Spec.Phase, sc.Generation))
	if sc.Spec.Phase == servicesv1alpha1.PhasePublished && newStatus.PublishedAt == nil {
		now := metav1.Now()
		newStatus.PublishedAt = &now
	}

	if !serviceConfigurationStatusNeedsUpdate(&sc.Status, newStatus) {
		return nil
	}
	sc.Status = *newStatus
	if err := r.Status().Update(ctx, sc); err != nil {
		return fmt.Errorf("update ServiceConfiguration status: %w", err)
	}
	return nil
}

func serviceConfigurationStatusNeedsUpdate(current, desired *servicesv1alpha1.ServiceConfigurationStatus) bool {
	if current.ObservedGeneration != desired.ObservedGeneration {
		return true
	}
	if (current.PublishedAt == nil) != (desired.PublishedAt == nil) {
		return true
	}
	for _, t := range []string{ConditionTypeReady, ConditionTypeBillingFanOutHealthy, ConditionTypePublished} {
		if !conditionsEqual(current.Conditions, desired.Conditions, t) {
			return true
		}
	}
	return false
}

// SetupWithManager wires the reconciler into the manager. Client, Scheme,
// and BillingFanOut are populated from the manager if not already set so
// tests can inject fakes without re-wiring.
func (r *ServiceConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	if r.BillingFanOut == nil {
		r.BillingFanOut = &BillingFanOut{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("serviceconfiguration").
		For(&servicesv1alpha1.ServiceConfiguration{}).
		Complete(r)
}
