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

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const serviceConfigurationFinalizer = "services.miloapis.com/serviceconfiguration-protection"

const (
	// ConditionTypeBillingFanOutHealthy surfaces whether the billing
	// fan-out is up to date with the current ServiceConfiguration spec.
	ConditionTypeBillingFanOutHealthy = "BillingFanOutHealthy"

	// ConditionTypeQuotaFanOutHealthy surfaces whether the quota
	// fan-out is up to date with the current ServiceConfiguration spec.
	ConditionTypeQuotaFanOutHealthy = "QuotaFanOutHealthy"

	// ConditionTypePricingFanOutHealthy surfaces whether the pricing
	// fan-out is up to date with the current ServiceConfiguration spec.
	ConditionTypePricingFanOutHealthy = "PricingFanOutHealthy"

	// ConditionTypeChargeFanOutHealthy surfaces whether the charge
	// fan-out is up to date with the current ServiceConfiguration spec.
	ConditionTypeChargeFanOutHealthy = "ChargeFanOutHealthy"

	reasonServiceConfigurationReady = "ServiceConfigurationReady"
	reasonServiceRefNotFound        = "ServiceRefNotFound"
	reasonBillingFanOutFailed       = "BillingFanOutFailed"
	reasonBillingFanOutHealthy      = "BillingFanOutHealthy"
	reasonBillingFanOutSkipped      = "BillingFanOutSkipped"
	reasonQuotaFanOutFailed         = "QuotaFanOutFailed"
	reasonQuotaFanOutHealthy        = "QuotaFanOutHealthy"
	reasonQuotaFanOutSkipped        = "QuotaFanOutSkipped"
	reasonPricingFanOutFailed       = "PricingFanOutFailed"
	reasonPricingFanOutHealthy      = "PricingFanOutHealthy"
	reasonPricingFanOutSkipped      = "PricingFanOutSkipped"
	reasonChargeFanOutFailed        = "ChargeFanOutFailed"
	reasonChargeFanOutHealthy       = "ChargeFanOutHealthy"
	reasonChargeFanOutSkipped       = "ChargeFanOutSkipped"
)

// ServiceConfigurationReconciler reconciles a ServiceConfiguration
// object. It owns the billing, pricing, charge, and quota fan-outs:
// changes to the document materialize as downstream CRDs via server-side
// apply, and previously-managed objects no longer in the desired set are
// deleted.
type ServiceConfigurationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	BillingFanOut *BillingFanOut
	QuotaFanOut   *QuotaFanOut
	PricingFanOut *PricingFanOut
	ChargeFanOut  *ChargeFanOut
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions;monitoredresourcetypes;servicepricings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quota.miloapis.com,resources=resourceregistrations;claimcreationpolicies,verbs=get;list;watch;create;update;patch;delete

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
			readyMsg := fmt.Sprintf("The service %q this configuration belongs to could not be found.", sc.Spec.ServiceRef.Name)
			fanOutMsg := fmt.Sprintf("Can't set up billing and quota until the service %q this configuration belongs to exists.", sc.Spec.ServiceRef.Name)
			return ctrl.Result{}, r.writeStatusConditions(ctx, &sc, "",
				metav1.Condition{
					Type:               ConditionTypeReady,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            readyMsg,
				},
				metav1.Condition{
					Type:               ConditionTypeBillingFanOutHealthy,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            fanOutMsg,
				},
				metav1.Condition{
					Type:               ConditionTypeQuotaFanOutHealthy,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            fanOutMsg,
				},
				metav1.Condition{
					Type:               ConditionTypePricingFanOutHealthy,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            fanOutMsg,
				},
				metav1.Condition{
					Type:               ConditionTypeChargeFanOutHealthy,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sc.Generation,
					Reason:             reasonServiceRefNotFound,
					Message:            fanOutMsg,
				},
			)
		}
		return ctrl.Result{}, fmt.Errorf("fetch referenced Service %q: %w", sc.Spec.ServiceRef.Name, err)
	}

	var billingFanOutErr error
	if sc.Spec.Phase != servicesv1alpha1.PhaseDraft {
		billingFanOutErr = r.BillingFanOut.Reconcile(ctx, &sc)
	}
	billingFanOutCondition := desiredBillingFanOutCondition(&sc, billingFanOutErr)

	var quotaFanOutErr error
	if sc.Spec.Phase != servicesv1alpha1.PhaseDraft {
		quotaFanOutErr = r.QuotaFanOut.Reconcile(ctx, &sc)
	}
	quotaFanOutCondition := desiredQuotaFanOutCondition(&sc, quotaFanOutErr)

	var pricingFanOutErr error
	if sc.Spec.Phase != servicesv1alpha1.PhaseDraft {
		pricingFanOutErr = r.PricingFanOut.Reconcile(ctx, &sc)
	}
	pricingFanOutCondition := desiredPricingFanOutCondition(&sc, pricingFanOutErr)

	var chargeFanOutErr error
	if sc.Spec.Phase != servicesv1alpha1.PhaseDraft {
		chargeFanOutErr = r.ChargeFanOut.Reconcile(ctx, &sc)
	}
	chargeFanOutCondition := desiredChargeFanOutCondition(&sc, chargeFanOutErr)

	readyCondition := metav1.Condition{
		Type:               ConditionTypeReady,
		ObservedGeneration: sc.Generation,
	}

	switch {
	case billingFanOutErr != nil:
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonBillingFanOutFailed
		readyCondition.Message = billingFanOutCondition.Message
	case quotaFanOutErr != nil:
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonQuotaFanOutFailed
		readyCondition.Message = quotaFanOutCondition.Message
	case pricingFanOutErr != nil:
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonPricingFanOutFailed
		readyCondition.Message = pricingFanOutCondition.Message
	case chargeFanOutErr != nil:
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonChargeFanOutFailed
		readyCondition.Message = chargeFanOutCondition.Message
	default:
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = reasonServiceConfigurationReady
		readyCondition.Message = "Service configuration is ready; billing, pricing, and quota are set up."
	}

	if err := r.writeStatusConditions(ctx, &sc, svc.Spec.ServiceName,
		readyCondition, billingFanOutCondition, quotaFanOutCondition,
		pricingFanOutCondition, chargeFanOutCondition,
	); err != nil {
		return ctrl.Result{}, err
	}

	if billingFanOutErr != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceConfiguration: %w", billingFanOutErr)
	}
	if quotaFanOutErr != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceConfiguration: %w", quotaFanOutErr)
	}
	if pricingFanOutErr != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceConfiguration: %w", pricingFanOutErr)
	}
	if chargeFanOutErr != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceConfiguration: %w", chargeFanOutErr)
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
	if err := r.QuotaFanOut.Cleanup(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup quota objects: %w", err)
	}
	if err := r.PricingFanOut.Cleanup(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup pricing objects: %w", err)
	}
	if err := r.ChargeFanOut.Cleanup(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup charge objects: %w", err)
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
	serviceName string,
	conds ...metav1.Condition,
) error {
	newStatus := sc.Status.DeepCopy()
	newStatus.ObservedGeneration = sc.Generation
	if serviceName != "" {
		newStatus.ServiceName = serviceName
	}
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
	if current.ServiceName != desired.ServiceName {
		return true
	}
	if (current.PublishedAt == nil) != (desired.PublishedAt == nil) {
		return true
	}
	for _, t := range []string{
		ConditionTypeReady,
		ConditionTypeBillingFanOutHealthy,
		ConditionTypeQuotaFanOutHealthy,
		ConditionTypePricingFanOutHealthy,
		ConditionTypeChargeFanOutHealthy,
		ConditionTypePublished,
	} {
		if !conditionsEqual(current.Conditions, desired.Conditions, t) {
			return true
		}
	}
	return false
}

func desiredBillingFanOutCondition(sc *servicesv1alpha1.ServiceConfiguration, err error) metav1.Condition {
	c := metav1.Condition{
		Type:               ConditionTypeBillingFanOutHealthy,
		ObservedGeneration: sc.Generation,
	}
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonBillingFanOutSkipped
		c.Message = "Billing setup is on hold while this configuration is still a draft."
		return c
	}
	if err != nil {
		c.Status = metav1.ConditionFalse
		c.Reason = reasonBillingFanOutFailed
		c.Message = "Couldn't finish setting up billing for this service; the system will keep retrying."
	} else {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonBillingFanOutHealthy
		c.Message = "Billing is set up for this service."
	}
	return c
}

func desiredQuotaFanOutCondition(sc *servicesv1alpha1.ServiceConfiguration, err error) metav1.Condition {
	c := metav1.Condition{
		Type:               ConditionTypeQuotaFanOutHealthy,
		ObservedGeneration: sc.Generation,
	}
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonQuotaFanOutSkipped
		c.Message = "Quota setup is on hold while this configuration is still a draft."
		return c
	}
	if err != nil {
		c.Status = metav1.ConditionFalse
		c.Reason = reasonQuotaFanOutFailed
		c.Message = "Couldn't finish setting up quotas for this service; the system will keep retrying."
	} else {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonQuotaFanOutHealthy
		c.Message = "Quotas are set up for this service."
	}
	return c
}

func desiredPricingFanOutCondition(sc *servicesv1alpha1.ServiceConfiguration, err error) metav1.Condition {
	c := metav1.Condition{
		Type:               ConditionTypePricingFanOutHealthy,
		ObservedGeneration: sc.Generation,
	}
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonPricingFanOutSkipped
		c.Message = "Pricing setup is on hold while this configuration is still a draft."
		return c
	}
	if err != nil {
		c.Status = metav1.ConditionFalse
		c.Reason = reasonPricingFanOutFailed
		c.Message = "Couldn't finish setting up pricing for this service; the system will keep retrying."
	} else {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonPricingFanOutHealthy
		c.Message = "Pricing is set up for this service."
	}
	return c
}

func desiredChargeFanOutCondition(sc *servicesv1alpha1.ServiceConfiguration, err error) metav1.Condition {
	c := metav1.Condition{
		Type:               ConditionTypeChargeFanOutHealthy,
		ObservedGeneration: sc.Generation,
	}
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonChargeFanOutSkipped
		c.Message = "Fixed-charge setup is on hold while this configuration is still a draft."
		return c
	}
	if err != nil {
		c.Status = metav1.ConditionFalse
		c.Reason = reasonChargeFanOutFailed
		c.Message = "Couldn't finish setting up fixed charges for this service; the system will keep retrying."
	} else {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonChargeFanOutHealthy
		c.Message = "Fixed charges are set up for this service."
	}
	return c
}

// SetupWithManager wires the reconciler into the manager. Client, Scheme,
// and fan-outs are populated from the manager if not already set so
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
		}
	}
	if r.QuotaFanOut == nil {
		r.QuotaFanOut = &QuotaFanOut{
			Client:     mgr.GetClient(),
			Scheme:     mgr.GetScheme(),
			RESTMapper: mgr.GetRESTMapper(),
		}
	}
	if r.PricingFanOut == nil {
		r.PricingFanOut = &PricingFanOut{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorder("pricing-fanout"),
		}
	}
	if r.ChargeFanOut == nil {
		r.ChargeFanOut = &ChargeFanOut{
			Client: mgr.GetClient(),
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("serviceconfiguration").
		For(&servicesv1alpha1.ServiceConfiguration{}).
		Complete(r)
}
