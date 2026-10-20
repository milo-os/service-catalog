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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ServiceAgentReconciler tells a provider whether its agent's request to reach
// every project is honoured. An agent may ask for spec.visibility.entitlement
// None, but only the platform owner's allowlist (EntitlementFreeAgents, from
// operator config) makes that take effect. Without this, an agent that asked
// and was not allowlisted would simply never appear in unentitled projects,
// with nothing to say why.
//
// It records the answer as the EntitlementWaived condition on the agent, on
// the root cluster, and writes only when the answer changes — never per
// project and never on a timer.
type ServiceAgentReconciler struct {
	client client.Client

	// EntitlementFreeAgents is the same allowlist the CapabilityBinding
	// controller honours.
	EntitlementFreeAgents map[string]struct{}
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents/status,verbs=get;update;patch

func (r *ServiceAgentReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var agent servicesv1alpha1.ServiceAgent
	if err := r.client.Get(ctx, req.NamespacedName, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	conditions := append([]metav1.Condition(nil), agent.Status.Conditions...)
	var changed bool
	if agent.RequestsNoEntitlement() {
		changed = apimeta.SetStatusCondition(&conditions, r.entitlementWaivedCondition(&agent))
	} else {
		// Nothing was asked, so there is nothing to answer.
		changed = apimeta.RemoveStatusCondition(&conditions, servicesv1alpha1.ConditionTypeEntitlementWaived)
	}
	if !changed {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.Conditions = conditions
	if err := r.client.Status().Patch(ctx, &agent, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update ServiceAgent status: %w", err)
	}

	cond := apimeta.FindStatusCondition(conditions, servicesv1alpha1.ConditionTypeEntitlementWaived)
	if cond != nil && cond.Status == metav1.ConditionFalse {
		log.FromContext(ctx).Info("agent asks to reach every project but is not allowlisted; it reaches only entitled projects",
			"agent", agent.Name)
	}
	return ctrl.Result{}, nil
}

func (r *ServiceAgentReconciler) entitlementWaivedCondition(agent *servicesv1alpha1.ServiceAgent) metav1.Condition {
	if _, ok := r.EntitlementFreeAgents[agent.Name]; ok {
		return metav1.Condition{
			Type:    servicesv1alpha1.ConditionTypeEntitlementWaived,
			Status:  metav1.ConditionTrue,
			Reason:  servicesv1alpha1.ReasonAllowlisted,
			Message: "The platform owner has allowlisted this agent, so it reaches every project the operator projects into, without an entitlement.",
		}
	}
	return metav1.Condition{
		Type:   servicesv1alpha1.ConditionTypeEntitlementWaived,
		Status: metav1.ConditionFalse,
		Reason: servicesv1alpha1.ReasonNotAllowlisted,
		Message: "spec.visibility.entitlement is None, but the platform owner has not allowlisted this agent, " +
			"so it reaches only projects with an Active entitlement. Ask the platform owner to add it to " +
			"the services operator's capabilities.entitlementFreeAgents.",
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("serviceagent").
		For(&servicesv1alpha1.ServiceAgent{}).
		Complete(r)
}
