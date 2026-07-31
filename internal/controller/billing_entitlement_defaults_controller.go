// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// billingServiceCanonicalName is the canonical serviceName of the
	// platform billing service whose ServiceConfiguration carries
	// spec.defaultOffer.
	billingServiceCanonicalName = "billing.miloapis.com"

	billingEntitlementDefaultsFieldManager = "services-operator-billing-entitlement-defaults"

	// labelBillingEntitlementDefault marks BillingEntitlements seeded by
	// this controller so they are distinguishable from staff-authored ones.
	labelBillingEntitlementDefault = "services.miloapis.com/billing-entitlement-default"
)

// BillingEntitlementDefaultsReconciler seeds a default BillingEntitlement
// for every BillingAccount from the billing service's Published
// ServiceConfiguration.spec.defaultOffer.
//
// Create-once semantics: once a BillingEntitlement named be-<uid>-default
// exists for an account, this controller leaves it alone so staff can
// change offerRef without being overwritten on the next reconcile.
type BillingEntitlementDefaultsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services;serviceconfigurations,verbs=get;list;watch

func (r *BillingEntitlementDefaultsReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ba billingv1alpha1.BillingAccount
	if err := r.Get(ctx, req.NamespacedName, &ba); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetch BillingAccount: %w", err)
	}

	if !ba.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	defaultOffer, err := r.lookupBillingDefaultOffer(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if defaultOffer == "" {
		logger.V(1).Info("no defaultOffer configured on billing ServiceConfiguration; skipping")
		return ctrl.Result{}, nil
	}

	// One active BillingEntitlement per account (billing admission). If
	// staff already authored a BE under any name, do not try to seed
	// be-<uid>-default — Apply would fail forever on that uniqueness rule.
	existing, err := r.anyBillingEntitlementForAccount(ctx, ba.Namespace, ba.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if existing != nil {
		logger.V(1).Info("BillingAccount already has a BillingEntitlement; skipping default seed",
			"existing", existing.Name)
		return ctrl.Result{}, nil
	}

	entitlementName := defaultBillingEntitlementName(ba.UID)

	obj := &billingv1alpha1.BillingEntitlement{
		TypeMeta: metav1.TypeMeta{
			APIVersion: billingv1alpha1.GroupVersion.String(),
			Kind:       "BillingEntitlement",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      entitlementName,
			Namespace: ba.Namespace,
			Labels: map[string]string{
				labelManagedBy:                 labelManagedByValue,
				labelBillingEntitlementDefault: "true",
			},
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{
				Name: ba.Name,
			},
			OfferRef: billingv1alpha1.OfferReference{
				Name: defaultOffer,
			},
		},
	}

	if err := r.Patch(ctx, obj, client.Apply, //nolint:staticcheck // SA1019: migrate to client.Apply() with ApplyConfiguration in a follow-up
		client.FieldOwner(billingEntitlementDefaultsFieldManager),
		client.ForceOwnership,
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply BillingEntitlement %q for BillingAccount %q: %w",
			entitlementName, ba.Name, err)
	}

	logger.Info("applied default BillingEntitlement",
		"billingAccount", ba.Name,
		"namespace", ba.Namespace,
		"entitlement", entitlementName,
		"offer", defaultOffer,
	)
	return ctrl.Result{}, nil
}

// anyBillingEntitlementForAccount returns a non-deleting BillingEntitlement
// for the account, if one exists.
func (r *BillingEntitlementDefaultsReconciler) anyBillingEntitlementForAccount(
	ctx context.Context,
	namespace, billingAccountName string,
) (*billingv1alpha1.BillingEntitlement, error) {
	var list billingv1alpha1.BillingEntitlementList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list BillingEntitlements in %q: %w", namespace, err)
	}
	for i := range list.Items {
		be := &list.Items[i]
		if be.DeletionTimestamp.IsZero() && be.Spec.BillingAccountRef.Name == billingAccountName {
			return be, nil
		}
	}
	return nil, nil
}

// lookupBillingDefaultOffer resolves the billing.miloapis.com Service and
// returns the DefaultOffer from its Published ServiceConfiguration. Empty
// string means no-op (service or config missing, or DefaultOffer unset).
func (r *BillingEntitlementDefaultsReconciler) lookupBillingDefaultOffer(ctx context.Context) (string, error) {
	var svcList servicesv1alpha1.ServiceList
	if err := r.List(ctx, &svcList); err != nil {
		return "", fmt.Errorf("list Services: %w", err)
	}
	var billingSvc *servicesv1alpha1.Service
	for i := range svcList.Items {
		if svcList.Items[i].Spec.ServiceName == billingServiceCanonicalName {
			billingSvc = &svcList.Items[i]
			break
		}
	}
	if billingSvc == nil {
		return "", nil
	}

	var scList servicesv1alpha1.ServiceConfigurationList
	if err := r.List(ctx, &scList); err != nil {
		return "", fmt.Errorf("list ServiceConfigurations: %w", err)
	}
	for i := range scList.Items {
		sc := &scList.Items[i]
		if sc.Spec.ServiceRef.Name != billingSvc.Name {
			continue
		}
		if sc.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}
		if sc.Spec.DefaultOffer != "" {
			return sc.Spec.DefaultOffer, nil
		}
	}
	return "", nil
}

// defaultBillingEntitlementName returns the deterministic name for the
// default BillingEntitlement seeded for a BillingAccount.
func defaultBillingEntitlementName(uid types.UID) string {
	return fmt.Sprintf("be-%s-default", uid)
}

// SetupWithManager wires the reconciler into the manager.
func (r *BillingEntitlementDefaultsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("billing-entitlement-defaults").
		For(&billingv1alpha1.BillingAccount{}).
		Watches(
			&servicesv1alpha1.ServiceConfiguration{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueBillingAccountsForDefaultOffer),
		).
		Complete(r)
}

// enqueueBillingAccountsForDefaultOffer re-enqueues every BillingAccount when
// the billing service's ServiceConfiguration changes so a newly published
// defaultOffer lands on accounts that do not yet have a BillingEntitlement.
// Other ServiceConfigurations are ignored to avoid a thundering herd.
func (r *BillingEntitlementDefaultsReconciler) enqueueBillingAccountsForDefaultOffer(ctx context.Context, obj client.Object) []reconcile.Request {
	sc, ok := obj.(*servicesv1alpha1.ServiceConfiguration)
	if !ok || sc == nil {
		return nil
	}
	if !r.isBillingServiceConfiguration(ctx, sc) {
		return nil
	}

	var list billingv1alpha1.BillingAccountList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "list BillingAccounts for fan-out re-enqueue")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      list.Items[i].Name,
				Namespace: list.Items[i].Namespace,
			},
		})
	}
	return out
}

// isBillingServiceConfiguration reports whether sc belongs to the
// billing.miloapis.com Service (the only place defaultOffer is read).
func (r *BillingEntitlementDefaultsReconciler) isBillingServiceConfiguration(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) bool {
	if sc.Spec.ServiceRef.Name == "" {
		return false
	}
	var svc servicesv1alpha1.Service
	if err := r.Get(ctx, client.ObjectKey{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		return false
	}
	return svc.Spec.ServiceName == billingServiceCanonicalName
}
