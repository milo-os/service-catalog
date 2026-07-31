// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	billingapply "go.miloapis.com/billing/applyconfiguration/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const chargesFieldManagerName = "services-operator-charges"

// ChargeFanOut materializes OneTime/Recurring ServicePricing objects declared
// by ServiceConfiguration.spec.charges[] via server-side apply and prunes
// previously-applied objects that no longer appear in the desired set.
type ChargeFanOut struct {
	Client client.Client
}

// Reconcile applies every fixed-charge ServicePricing declared by sc and
// deletes any previously-managed OneTime/Recurring ServicePricing owned by sc
// that is no longer in the desired set. Draft configurations are skipped.
func (f *ChargeFanOut) Reconcile(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
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
	return f.pruneServicePricings(ctx, sc, desired)
}

// Cleanup deletes every OneTime/Recurring ServicePricing owned by sc. Used
// during finalization to release managed state before the owner record goes
// away.
func (f *ChargeFanOut) Cleanup(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	return f.pruneServicePricings(ctx, sc, nil)
}

func (f *ChargeFanOut) resolveServiceName(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) (string, error) {
	var svc servicesv1alpha1.Service
	if err := f.Client.Get(ctx, client.ObjectKey{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		return "", fmt.Errorf("resolve Service %q: %w", sc.Spec.ServiceRef.Name, err)
	}
	return svc.Spec.ServiceName, nil
}

func (f *ChargeFanOut) applyServicePricings(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName string,
) (map[string]struct{}, error) {
	desired := make(map[string]struct{}, len(sc.Spec.Charges))
	for i := range sc.Spec.Charges {
		charge := &sc.Spec.Charges[i]
		name := encodeServicePricingName(charge.Name)
		currency := charge.Currency
		if currency == "" {
			currency = "USD"
		}

		spec := billingapply.ServicePricingSpec().
			WithChargeType(billingv1alpha1.ChargeType(charge.ChargeType)).
			WithServiceRef(serviceName).
			WithCurrency(currency).
			WithDisplayName(charge.DisplayName).
			WithAmount(charge.Amount)
		if charge.Trigger != "" {
			spec = spec.WithTrigger(billingv1alpha1.ChargeTrigger(charge.Trigger))
		}
		if charge.Interval != "" {
			spec = spec.WithInterval(billingv1alpha1.ChargeInterval(charge.Interval))
		}

		applyConfig := billingapply.ServicePricing(name, billingv1alpha1.DefaultServicePricingNamespace).
			WithLabels(map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: serviceName,
			}).
			WithOwnerReferences(controllerRef(sc)).
			WithSpec(spec)
		if err := f.Client.Apply(ctx, applyConfig,
			client.FieldOwner(chargesFieldManagerName),
			client.ForceOwnership,
		); err != nil {
			return nil, fmt.Errorf("apply ServicePricing %q: %w", name, err)
		}
		desired[name] = struct{}{}
	}
	return desired, nil
}

func (f *ChargeFanOut) pruneServicePricings(
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
		if obj.Spec.ChargeType != billingv1alpha1.ChargeTypeOneTime &&
			obj.Spec.ChargeType != billingv1alpha1.ChargeTypeRecurring {
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
