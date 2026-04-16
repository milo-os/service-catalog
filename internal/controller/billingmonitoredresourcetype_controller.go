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

// BillingMonitoredResourceTypeReconciler watches services.MonitoredResourceType
// and pushes a corresponding billing.MonitoredResourceType via server-side apply.
type BillingMonitoredResourceTypeReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=monitoredresourcetypes,verbs=get;list;watch;create;update;patch;delete

func (r *BillingMonitoredResourceTypeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mrt servicesv1alpha1.MonitoredResourceType
	if err := r.client.Get(ctx, req.NamespacedName, &mrt); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// On delete, the owner reference cascade will handle cleanup via
	// Kubernetes GC. No explicit finalizer or cleanup is needed here.
	if !mrt.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Skip Draft resources — only push Published, Deprecated, and Retired.
	if mrt.Spec.Phase == servicesv1alpha1.PhaseDraft {
		return ctrl.Result{}, nil
	}

	trueVal := true
	ownerRef := metav1.OwnerReference{
		APIVersion:         servicesv1alpha1.GroupVersion.String(),
		Kind:               "MonitoredResourceType",
		Name:               mrt.Name,
		UID:                mrt.UID,
		Controller:         &trueVal,
		BlockOwnerDeletion: &trueVal,
	}

	billingLabels := make([]billingv1alpha1.MonitoredResourceLabel, 0, len(mrt.Spec.Labels))
	for _, l := range mrt.Spec.Labels {
		billingLabels = append(billingLabels, billingv1alpha1.MonitoredResourceLabel{
			Name:          l.Name,
			Required:      l.Required,
			Description:   l.Description,
			AllowedValues: l.AllowedValues,
		})
	}

	billingMRT := &billingv1alpha1.MonitoredResourceType{
		TypeMeta: metav1.TypeMeta{
			APIVersion: billingv1alpha1.GroupVersion.String(),
			Kind:       "MonitoredResourceType",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: mrt.Name,
			Labels: map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: mrt.Spec.Owner.Service,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: billingv1alpha1.MonitoredResourceTypeSpec{
			ResourceTypeName: mrt.Spec.ResourceTypeName,
			Phase:            billingv1alpha1.Phase(mrt.Spec.Phase),
			DisplayName:      mrt.Spec.DisplayName,
			Description:      mrt.Spec.Description,
			GVK: billingv1alpha1.MonitoredResourceTypeGVK{
				Group: mrt.Spec.GVK.Group,
				Kind:  mrt.Spec.GVK.Kind,
			},
			Labels: billingLabels,
		},
	}

	if err := r.client.Patch(ctx, billingMRT, client.Apply, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply billing MonitoredResourceType: %w", err)
	}

	logger.Info("applied billing MonitoredResourceType",
		"name", mrt.Name,
		"resourceTypeName", mrt.Spec.ResourceTypeName,
		"phase", mrt.Spec.Phase,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BillingMonitoredResourceTypeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("billing-monitoredresourcetype-push").
		For(&servicesv1alpha1.MonitoredResourceType{}).
		Complete(r)
}
