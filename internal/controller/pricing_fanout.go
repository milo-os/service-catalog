// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	billingapply "go.miloapis.com/billing/applyconfiguration/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	pricingFieldManagerName = "services-operator-pricing"
)

// PricingFanOut materializes Usage ServicePricing objects declared by
// ServiceConfiguration.spec.metrics[].pricing via server-side apply and
// prunes previously-applied objects that no longer appear in the desired set.
type PricingFanOut struct {
	Client   client.Client
	Recorder events.EventRecorder
}

// Reconcile applies every Usage ServicePricing declared by sc and deletes any
// previously-managed Usage ServicePricing owned by sc that is no longer in the
// desired set. Draft configurations are skipped.
func (f *PricingFanOut) Reconcile(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		return nil
	}

	serviceName, err := f.resolveServiceName(ctx, sc)
	if err != nil {
		return err
	}

	desired, err := f.applyServicePricings(ctx, sc, serviceName)
	if err != nil {
		return err
	}

	if err := f.pruneServicePricings(ctx, sc, desired); err != nil {
		return err
	}

	if len(desired) > 0 && usesOrganizationDefaultQuotaGating(sc) {
		// Log rather than Event: emitting a Warning on every reconcile
		// spams the event stream for every priced SC that has not opted
		// into BillingEntitlement gating.
		log.FromContext(ctx).Info(
			"ServicePricing resources were created but billing.quotaGating is OrganizationDefault; "+
				"accounts may be billed without being blocked when they lack an active Offer. "+
				"Set billing.quotaGating to BillingEntitlement to opt in.",
			"serviceConfiguration", sc.Name,
		)
	}

	return nil
}

// Cleanup deletes every Usage ServicePricing owned by sc. Used during
// finalization to release managed state before the owner record goes away.
func (f *PricingFanOut) Cleanup(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	return f.pruneServicePricings(ctx, sc, nil)
}

func (f *PricingFanOut) resolveServiceName(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) (string, error) {
	var svc servicesv1alpha1.Service
	if err := f.Client.Get(ctx, client.ObjectKey{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		return "", fmt.Errorf("resolve Service %q: %w", sc.Spec.ServiceRef.Name, err)
	}
	return svc.Spec.ServiceName, nil
}

func (f *PricingFanOut) applyServicePricings(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName string,
) (map[string]struct{}, error) {
	desired := make(map[string]struct{})
	for i := range sc.Spec.Metrics {
		metric := &sc.Spec.Metrics[i]
		if metric.Pricing == nil {
			continue
		}

		name := encodeServicePricingName(metric.Name)
		currency := metric.Pricing.Currency
		if currency == "" {
			currency = "USD"
		}

		applyConfig := billingapply.ServicePricing(name, billingv1alpha1.DefaultServicePricingNamespace).
			WithLabels(map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: serviceName,
			}).
			WithOwnerReferences(controllerRef(sc)).
			WithSpec(billingapply.ServicePricingSpec().
				WithChargeType(billingv1alpha1.ChargeTypeUsage).
				WithServiceRef(serviceName).
				WithCurrency(currency).
				WithDisplayName(metric.DisplayName).
				WithMetric(metric.Name).
				WithPricingUnit(metric.Pricing.PricingUnit).
				WithRates(billingPricingRatesApplyFor(metric.Pricing.Rates)...),
			)
		if err := f.Client.Apply(ctx, applyConfig,
			client.FieldOwner(pricingFieldManagerName),
			client.ForceOwnership,
		); err != nil {
			return nil, fmt.Errorf("apply ServicePricing %q: %w", name, err)
		}
		desired[name] = struct{}{}
	}
	return desired, nil
}

func (f *PricingFanOut) pruneServicePricings(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desired map[string]struct{},
) error {
	var list billingv1alpha1.ServicePricingList
	if err := f.Client.List(ctx, &list,
		client.InNamespace(billingv1alpha1.DefaultServicePricingNamespace),
		client.MatchingLabelsSelector{Selector: managedByFanoutSelector},
	); err != nil {
		return fmt.Errorf("list ServicePricings: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.Spec.ChargeType != billingv1alpha1.ChargeTypeUsage {
			continue
		}
		if !ownedBy(obj.OwnerReferences, sc.UID) {
			continue
		}
		if _, keep := desired[obj.Name]; keep {
			continue
		}
		if err := f.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ServicePricing %q: %w", obj.Name, err)
		}
	}
	return nil
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

func billingPricingRatesApplyFor(rates []servicesv1alpha1.PricingRateEntry) []*billingapply.PricingRateApplyConfiguration {
	if len(rates) == 0 {
		return nil
	}
	out := make([]*billingapply.PricingRateApplyConfiguration, 0, len(rates))
	for _, r := range rates {
		pr := billingapply.PricingRate()
		if r.Flat != "" {
			pr = pr.WithFlat(r.Flat)
		}
		if len(r.Tiered) > 0 {
			pr = pr.WithTiered(billingPricingTiersApplyFor(r.Tiered)...)
		}
		if r.Match != nil {
			pr = pr.WithMatch(billingapply.DimensionMatch().
				WithDimension(r.Match.Dimension).
				WithValue(r.Match.Value),
			)
		}
		out = append(out, pr)
	}
	return out
}

func billingPricingTiersApplyFor(bands []servicesv1alpha1.PricingTierBand) []*billingapply.PricingTierBandApplyConfiguration {
	if len(bands) == 0 {
		return nil
	}
	out := make([]*billingapply.PricingTierBandApplyConfiguration, 0, len(bands))
	for _, b := range bands {
		band := billingapply.PricingTierBand().WithRate(b.Rate)
		if b.UpTo != "" {
			band = band.WithUpTo(b.UpTo)
		}
		out = append(out, band)
	}
	return out
}
