// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

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

	// migrateFromOfferRequeue is how long to wait before retrying the
	// remaining-offer scan after migrating one account. Parallel
	// reconciles can race on the last matching entitlements; a short
	// requeue lets the last writer observe remaining==0 and clear the
	// one-shot field.
	migrateFromOfferRequeue = 5 * time.Second
)

// billingDefaultConfig is the Published billing ServiceConfiguration's
// default-Offer settings used by this reconciler.
type billingDefaultConfig struct {
	DefaultOffer     string
	MigrateFromOffer string
	Config           *servicesv1alpha1.ServiceConfiguration
}

// BillingEntitlementDefaultsReconciler seeds a default BillingEntitlement
// for every BillingAccount from the billing service's Published
// ServiceConfiguration.spec.defaultOffer.
//
// Create-once semantics: once a BillingEntitlement exists for an
// account, this controller does not overwrite offerRef — except when
// spec.migrateFromOffer is set and the entitlement still points at that
// previous default. Custom offers are never touched. After no matching
// entitlements remain, migrateFromOffer is cleared.
type BillingEntitlementDefaultsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch;update;patch

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

	cfg, err := r.lookupBillingDefaultConfig(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cfg == nil || cfg.DefaultOffer == "" {
		logger.V(1).Info("no defaultOffer configured on billing ServiceConfiguration; skipping")
		return ctrl.Result{}, nil
	}

	existing, err := r.anyBillingEntitlementForAccount(ctx, ba.Namespace, ba.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	didMigrate := false
	switch {
	case existing == nil:
		if err := r.seedDefaultEntitlement(ctx, &ba, cfg.DefaultOffer); err != nil {
			return ctrl.Result{}, err
		}
	case cfg.MigrateFromOffer != "" &&
		existing.Spec.OfferRef.Name == cfg.MigrateFromOffer &&
		cfg.DefaultOffer != cfg.MigrateFromOffer:
		if err := r.migrateEntitlementOffer(ctx, existing, cfg.DefaultOffer); err != nil {
			return ctrl.Result{}, err
		}
		didMigrate = true
		logger.Info("migrated BillingEntitlement to defaultOffer",
			"billingAccount", ba.Name,
			"namespace", ba.Namespace,
			"entitlement", existing.Name,
			"from", cfg.MigrateFromOffer,
			"to", cfg.DefaultOffer,
		)
	default:
		logger.V(1).Info("BillingAccount already has a BillingEntitlement; skipping default seed",
			"existing", existing.Name)
	}

	if cfg.MigrateFromOffer == "" {
		return ctrl.Result{}, nil
	}

	remaining, err := r.countEntitlementsOnOffer(ctx, cfg.MigrateFromOffer)
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining == 0 {
		if err := r.clearMigrateFromOffer(ctx, cfg.Config, cfg.MigrateFromOffer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if didMigrate {
		return ctrl.Result{RequeueAfter: migrateFromOfferRequeue}, nil
	}
	return ctrl.Result{}, nil
}

func (r *BillingEntitlementDefaultsReconciler) seedDefaultEntitlement(
	ctx context.Context,
	ba *billingv1alpha1.BillingAccount,
	defaultOffer string,
) error {
	logger := log.FromContext(ctx)
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
		return fmt.Errorf("apply BillingEntitlement %q for BillingAccount %q: %w",
			entitlementName, ba.Name, err)
	}

	logger.Info("applied default BillingEntitlement",
		"billingAccount", ba.Name,
		"namespace", ba.Namespace,
		"entitlement", entitlementName,
		"offer", defaultOffer,
	)
	return nil
}

func (r *BillingEntitlementDefaultsReconciler) migrateEntitlementOffer(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
	toOffer string,
) error {
	original := be.DeepCopy()
	be.Spec.OfferRef.Name = toOffer
	if err := r.Patch(ctx, be, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("migrate BillingEntitlement %q offerRef to %q: %w", be.Name, toOffer, err)
	}
	return nil
}

func (r *BillingEntitlementDefaultsReconciler) countEntitlementsOnOffer(
	ctx context.Context,
	offerName string,
) (int, error) {
	var list billingv1alpha1.BillingEntitlementList
	if err := r.List(ctx, &list); err != nil {
		return 0, fmt.Errorf("list BillingEntitlements: %w", err)
	}
	n := 0
	for i := range list.Items {
		be := &list.Items[i]
		if be.DeletionTimestamp.IsZero() && be.Spec.OfferRef.Name == offerName {
			n++
		}
	}
	return n, nil
}

func (r *BillingEntitlementDefaultsReconciler) clearMigrateFromOffer(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	expectedFrom string,
) error {
	var live servicesv1alpha1.ServiceConfiguration
	if err := r.Get(ctx, client.ObjectKeyFromObject(sc), &live); err != nil {
		return fmt.Errorf("fetch ServiceConfiguration %q to clear migrateFromOffer: %w", sc.Name, err)
	}
	if live.Spec.MigrateFromOffer != expectedFrom {
		return nil
	}
	original := live.DeepCopy()
	live.Spec.MigrateFromOffer = ""
	if err := r.Patch(ctx, &live, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("clear migrateFromOffer on ServiceConfiguration %q: %w", live.Name, err)
	}
	log.FromContext(ctx).Info("cleared migrateFromOffer; no matching BillingEntitlements remain",
		"serviceConfiguration", live.Name,
		"from", expectedFrom,
	)
	return nil
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

// lookupBillingDefaultConfig resolves the billing.miloapis.com Service and
// returns defaultOffer / migrateFromOffer from its Published
// ServiceConfiguration. Nil means no-op (service or config missing, or
// DefaultOffer unset).
func (r *BillingEntitlementDefaultsReconciler) lookupBillingDefaultConfig(ctx context.Context) (*billingDefaultConfig, error) {
	var svcList servicesv1alpha1.ServiceList
	if err := r.List(ctx, &svcList); err != nil {
		return nil, fmt.Errorf("list Services: %w", err)
	}
	var billingSvc *servicesv1alpha1.Service
	for i := range svcList.Items {
		if svcList.Items[i].Spec.ServiceName == billingServiceCanonicalName {
			billingSvc = &svcList.Items[i]
			break
		}
	}
	if billingSvc == nil {
		return nil, nil
	}

	var scList servicesv1alpha1.ServiceConfigurationList
	if err := r.List(ctx, &scList); err != nil {
		return nil, fmt.Errorf("list ServiceConfigurations: %w", err)
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
			return &billingDefaultConfig{
				DefaultOffer:     sc.Spec.DefaultOffer,
				MigrateFromOffer: sc.Spec.MigrateFromOffer,
				Config:           sc.DeepCopy(),
			}, nil
		}
	}
	return nil, nil
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
// defaultOffer lands on accounts that do not yet have a BillingEntitlement
// and a migrateFromOffer one-shot can patch matching entitlements.
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
