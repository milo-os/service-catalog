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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// ConditionTypeMetricsFanOutHealthy surfaces whether the metrics
	// fan-out to billing MeterDefinitions is up to date with the current
	// ServiceAgentConfiguration spec.
	ConditionTypeMetricsFanOutHealthy = "MetricsFanOutHealthy"

	reasonServiceAgentConfigurationReady = "ServiceAgentConfigurationReady"
	reasonServiceAgentRefNotFound        = "ServiceAgentRefNotFound"
	reasonMetricsFanOutSkipped           = "MetricsFanOutSkipped"
	reasonMetricsFanOutStubbed           = "MetricsFanOutStubbed"
)

// ServiceAgentConfigurationReconciler reconciles a
// ServiceAgentConfiguration object: it resolves the serviceAgentRef
// chain (agent -> service) into status.serviceName and mirrors the
// declared phase into conditions, following
// ServiceConfigurationReconciler.
//
// TODO(agent-framework): fan agent metrics out to
// billing.miloapis.com/MeterDefinition objects, following the
// BillingFanOut pattern. Deliberately stubbed for the v0 slice:
// MeterDefinition.spec.monitoredResourceTypes requires MinItems=1 and
// agent configurations declare no monitored resource types or billing
// destinations, so a real fan-out first needs a product decision on the
// monitored resource type agent usage attributes to. The local usage
// sink asserts usage events, not MeterDefinitions. Until then the
// MetricsFanOutHealthy condition reports the stub explicitly. No
// finalizer is added: it exists on ServiceConfiguration solely to clean
// up fanned-out objects, and the stub creates none — add it together
// with the real fan-out.
type ServiceAgentConfigurationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagentconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagentconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents,verbs=get;list;watch

func (r *ServiceAgentConfigurationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sac servicesv1alpha1.ServiceAgentConfiguration
	if err := r.Get(ctx, req.NamespacedName, &sac); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetch ServiceAgentConfiguration: %w", err)
	}

	if !sac.DeletionTimestamp.IsZero() {
		// Nothing to finalize: the metrics fan-out is stubbed and no
		// downstream objects are created. See the TODO on the reconciler.
		return ctrl.Result{}, nil
	}

	var agent servicesv1alpha1.ServiceAgent
	if err := r.Get(ctx, client.ObjectKey{Name: sac.Spec.ServiceAgentRef.Name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("referenced ServiceAgent %q not found", sac.Spec.ServiceAgentRef.Name)
			return ctrl.Result{}, r.writeStatusConditions(ctx, &sac, "",
				metav1.Condition{
					Type:               ConditionTypeReady,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sac.Generation,
					Reason:             reasonServiceAgentRefNotFound,
					Message:            msg,
				},
				metav1.Condition{
					Type:               ConditionTypeMetricsFanOutHealthy,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: sac.Generation,
					Reason:             reasonServiceAgentRefNotFound,
					Message:            msg + "; cannot fan out",
				},
			)
		}
		return ctrl.Result{}, fmt.Errorf("fetch referenced ServiceAgent %q: %w", sac.Spec.ServiceAgentRef.Name, err)
	}

	// Resolve the canonical reverse-DNS service name through the agent's
	// serviceRef. Best-effort: a missing Service leaves serviceName
	// empty without failing the document — admission enforces the chain
	// at write time.
	serviceName := ""
	var svc servicesv1alpha1.Service
	if err := r.Get(ctx, client.ObjectKey{Name: agent.Spec.ServiceRef.Name}, &svc); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetch Service %q backing ServiceAgent %q: %w",
				agent.Spec.ServiceRef.Name, agent.Name, err)
		}
	} else {
		serviceName = svc.Spec.ServiceName
	}

	metricsFanOutCondition := desiredMetricsFanOutCondition(&sac)

	readyCondition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: sac.Generation,
		Reason:             reasonServiceAgentConfigurationReady,
		Message:            "ServiceAgentConfiguration is reconciled.",
	}

	if err := r.writeStatusConditions(ctx, &sac, serviceName, readyCondition, metricsFanOutCondition); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled serviceagentconfiguration",
		"name", sac.Name,
		"agent", agent.Name,
		"service", serviceName,
		"phase", sac.Spec.Phase,
	)
	return ctrl.Result{}, nil
}

func (r *ServiceAgentConfigurationReconciler) writeStatusConditions(
	ctx context.Context,
	sac *servicesv1alpha1.ServiceAgentConfiguration,
	serviceName string,
	conds ...metav1.Condition,
) error {
	newStatus := sac.Status.DeepCopy()
	newStatus.ObservedGeneration = sac.Generation
	if serviceName != "" {
		newStatus.ServiceName = serviceName
	}
	for _, c := range conds {
		apimeta.SetStatusCondition(&newStatus.Conditions, c)
	}
	apimeta.SetStatusCondition(&newStatus.Conditions, desiredPublishedCondition(sac.Spec.Phase, sac.Generation))
	if sac.Spec.Phase == servicesv1alpha1.PhasePublished && newStatus.PublishedAt == nil {
		now := metav1.Now()
		newStatus.PublishedAt = &now
	}

	if !serviceAgentConfigurationStatusNeedsUpdate(&sac.Status, newStatus) {
		return nil
	}
	sac.Status = *newStatus
	if err := r.Status().Update(ctx, sac); err != nil {
		return fmt.Errorf("update ServiceAgentConfiguration status: %w", err)
	}
	return nil
}

func serviceAgentConfigurationStatusNeedsUpdate(current, desired *servicesv1alpha1.ServiceAgentConfigurationStatus) bool {
	if current.ObservedGeneration != desired.ObservedGeneration {
		return true
	}
	if current.ServiceName != desired.ServiceName {
		return true
	}
	if (current.PublishedAt == nil) != (desired.PublishedAt == nil) {
		return true
	}
	for _, t := range []string{ConditionTypeReady, ConditionTypeMetricsFanOutHealthy, ConditionTypePublished} {
		if !conditionsEqual(current.Conditions, desired.Conditions, t) {
			return true
		}
	}
	return false
}

// desiredMetricsFanOutCondition mirrors desiredBillingFanOutCondition:
// Draft documents skip the fan-out; non-Draft documents report the stub
// until the real MeterDefinition fan-out lands (see the reconciler
// TODO).
func desiredMetricsFanOutCondition(sac *servicesv1alpha1.ServiceAgentConfiguration) metav1.Condition {
	c := metav1.Condition{
		Type:               ConditionTypeMetricsFanOutHealthy,
		ObservedGeneration: sac.Generation,
	}
	if sac.Spec.Phase == servicesv1alpha1.PhaseDraft {
		c.Status = metav1.ConditionTrue
		c.Reason = reasonMetricsFanOutSkipped
		c.Message = "ServiceAgentConfiguration is Draft; fan-out skipped."
		return c
	}
	c.Status = metav1.ConditionTrue
	c.Reason = reasonMetricsFanOutStubbed
	c.Message = "Metrics fan-out to billing MeterDefinitions is not implemented in this slice; usage events are asserted by the local capture sink."
	return c
}

// SetupWithManager wires the reconciler into the manager.
func (r *ServiceAgentConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("serviceagentconfiguration").
		For(&servicesv1alpha1.ServiceAgentConfiguration{}).
		Complete(r)
}
