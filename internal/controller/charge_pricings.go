// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// pruneChargePricings deletes the ServicePricings the retired charge
// fan-out made from sc's spec.charges. Prices are now standalone
// ServicePricing manifests in each service's repo, which carry neither the
// fan-out label nor an owner reference, so they are never touched here.
// Drop once every cluster has reconciled past this release.
func pruneChargePricings(ctx context.Context, c client.Client, sc *servicesv1alpha1.ServiceConfiguration) error {
	var list billingv1alpha1.ServicePricingList
	if err := c.List(ctx, &list,
		client.InNamespace(billingv1alpha1.DefaultServicePricingNamespace),
		client.MatchingLabelsSelector{Selector: managedByFanoutSelector},
	); err != nil {
		return fmt.Errorf("list ServicePricings: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if !ownedBy(obj.OwnerReferences, sc.UID) {
			continue
		}
		if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete charge ServicePricing %q: %w", obj.Name, err)
		}
	}
	return nil
}

// warnIfBilledWithoutGating logs when a Usage ServicePricing bills the
// service but its quota isn't gated on BillingEntitlement, so accounts can be
// billed without being blocked when they lack an active Offer. The service
// owner sets quota gating; billing sets the prices, so this only warns.
func warnIfBilledWithoutGating(
	ctx context.Context,
	c client.Client,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName string,
) {
	if !usesOrganizationDefaultQuotaGating(sc) {
		return
	}
	logger := log.FromContext(ctx)
	var list billingv1alpha1.ServicePricingList
	if err := c.List(ctx, &list, client.InNamespace(billingv1alpha1.DefaultServicePricingNamespace)); err != nil {
		logger.Error(err, "couldn't list ServicePricings to check quota gating", "serviceConfiguration", sc.Name)
		return
	}
	for _, sp := range list.Items {
		if sp.Spec.ChargeType != billingv1alpha1.ChargeTypeUsage || sp.Spec.ServiceRef != serviceName {
			continue
		}
		logger.Info(
			"A Usage ServicePricing bills this service but billing.quotaGating is OrganizationDefault; "+
				"accounts may be billed without being blocked when they lack an active Offer. "+
				"Set billing.quotaGating to BillingEntitlement to opt in.",
			"serviceConfiguration", sc.Name, "servicePricing", sp.Name,
		)
		return
	}
}

// usesOrganizationDefaultQuotaGating reports whether the SC leaves quota
// ungated on BillingEntitlement (explicit OrganizationDefault or the empty
// default).
func usesOrganizationDefaultQuotaGating(sc *servicesv1alpha1.ServiceConfiguration) bool {
	if sc.Spec.Billing == nil {
		return true
	}
	gating := sc.Spec.Billing.QuotaGating
	return gating == "" || gating == servicesv1alpha1.QuotaGatingOrganizationDefault
}
