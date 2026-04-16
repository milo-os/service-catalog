// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const (
	labelManagedBy      = "app.kubernetes.io/managed-by"
	labelManagedByValue = "services-operator"
	labelOwnerService   = "services.miloapis.com/service"
	fieldManagerName    = "services-operator"
)

// BillingMeterDefinitionReconciler watches services.MeterDefinition and
// pushes a corresponding billing.MeterDefinition via server-side apply.
type BillingMeterDefinitionReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions,verbs=get;list;watch;create;update;patch;delete

func (r *BillingMeterDefinitionReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var md servicesv1alpha1.MeterDefinition
	if err := r.client.Get(ctx, req.NamespacedName, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// On delete, the owner reference cascade will handle cleanup via
	// Kubernetes GC. No explicit finalizer or cleanup is needed here.
	if !md.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Skip Draft resources — only push Published, Deprecated, and Retired.
	if md.Spec.Phase == servicesv1alpha1.PhaseDraft {
		return ctrl.Result{}, nil
	}

	trueVal := true
	ownerRef := metav1.OwnerReference{
		APIVersion:         servicesv1alpha1.GroupVersion.String(),
		Kind:               "MeterDefinition",
		Name:               md.Name,
		UID:                md.UID,
		Controller:         &trueVal,
		BlockOwnerDeletion: &trueVal,
	}

	billingMD := &billingv1alpha1.MeterDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: billingv1alpha1.GroupVersion.String(),
			Kind:       "MeterDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: md.Name,
			Labels: map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: md.Spec.Owner.Service,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName:   md.Spec.MeterName,
			Phase:       billingv1alpha1.Phase(md.Spec.Phase),
			DisplayName: md.Spec.DisplayName,
			Description: md.Spec.Description,
			Measurement: billingv1alpha1.MeterMeasurement{
				Aggregation: billingv1alpha1.MeterAggregation(md.Spec.Measurement.Aggregation),
				Unit:        md.Spec.Measurement.Unit,
				Dimensions:  md.Spec.Measurement.Dimensions,
			},
			Billing: billingv1alpha1.MeterBilling{
				ConsumedUnit: md.Spec.Billing.ConsumedUnit,
				PricingUnit:  md.Spec.Billing.PricingUnit,
			},
		},
	}

	if err := r.client.Patch(ctx, billingMD, client.Apply, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply billing MeterDefinition: %w", err)
	}

	logger.Info("applied billing MeterDefinition",
		"name", md.Name,
		"meterName", md.Spec.MeterName,
		"phase", md.Spec.Phase,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BillingMeterDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("billing-meterdefinition-push").
		For(&servicesv1alpha1.MeterDefinition{}).
		Complete(r)
}
