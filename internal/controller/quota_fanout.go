// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const (
	quotaFieldManagerName = "services-operator-quota"
)

// QuotaFanOut materializes the downstream quota objects declared by
// a ServiceConfiguration (ResourceRegistration, ClaimCreationPolicy) via
// server-side apply and prunes previously-applied objects that no longer
// appear in the desired set.
type QuotaFanOut struct {
	Client     client.Client
	Scheme     *runtime.Scheme
	RESTMapper meta.RESTMapper
}

// Reconcile applies every quota object declared by sc and deletes any
// previously-managed quota object owned by sc that is no longer in the
// desired set. Draft configurations and nil quota specs are skipped.
func (f *QuotaFanOut) Reconcile(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		return nil
	}
	if sc.Spec.Quota == nil {
		return nil
	}

	desiredRRs, err := f.applyResourceRegistrations(ctx, sc)
	if err != nil {
		return err
	}
	desiredCCPs, err := f.applyClaimCreationPolicies(ctx, sc)
	if err != nil {
		return err
	}

	if err := f.pruneResourceRegistrations(ctx, sc, desiredRRs); err != nil {
		return err
	}
	return f.pruneClaimCreationPolicies(ctx, sc, desiredCCPs)
}

// Cleanup deletes every quota object owned by sc. Used during
// finalization to release managed state before the owner record goes away.
func (f *QuotaFanOut) Cleanup(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if err := f.pruneResourceRegistrations(ctx, sc, nil); err != nil {
		return err
	}
	return f.pruneClaimCreationPolicies(ctx, sc, nil)
}

func (f *QuotaFanOut) applyResourceRegistrations(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
) (map[string]struct{}, error) {
	// Build an index: metric name -> list of rule selectors that reference it
	// (for claimingResources on the ResourceRegistration).
	metricToSelectors := make(map[string][]quotav1alpha1.ClaimingResource)
	for i := range sc.Spec.Quota.MetricRules {
		rule := &sc.Spec.Quota.MetricRules[i]
		for metricName := range rule.MetricCosts {
			metricToSelectors[metricName] = append(metricToSelectors[metricName], quotav1alpha1.ClaimingResource{
				APIGroup: rule.Selector.APIGroup,
				Kind:     rule.Selector.Kind,
			})
		}
	}

	desired := make(map[string]struct{}, len(sc.Spec.Quota.Limits))
	for i := range sc.Spec.Quota.Limits {
		limit := &sc.Spec.Quota.Limits[i]
		name := encodeName(limit.Name)

		claimingResources := metricToSelectors[limit.Metric]
		// ResourceRegistrationSpec requires MinItems=1 for claimingResources;
		// if no metricRules reference this metric, we still need at least one
		// entry to pass validation. Use the consumerType itself as a placeholder.
		if len(claimingResources) == 0 {
			claimingResources = []quotav1alpha1.ClaimingResource{
				{
					APIGroup: limit.ConsumerType.APIGroup,
					Kind:     limit.ConsumerType.Kind,
				},
			}
		}

		obj := &quotav1alpha1.ResourceRegistration{
			TypeMeta: metav1.TypeMeta{
				APIVersion: quotav1alpha1.GroupVersion.String(),
				Kind:       "ResourceRegistration",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					labelManagedBy: labelManagedByValue,
				},
			},
			Spec: quotav1alpha1.ResourceRegistrationSpec{
				ResourceType: limit.Metric,
				ConsumerType: quotav1alpha1.ConsumerType{
					APIGroup: limit.ConsumerType.APIGroup,
					Kind:     limit.ConsumerType.Kind,
				},
				Type:                 "Entity",
				BaseUnit:             "1",
				DisplayUnit:          "1",
				UnitConversionFactor: 1,
				ClaimingResources:    claimingResources,
			},
		}
		if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
			return nil, fmt.Errorf("set controller ref on ResourceRegistration %q: %w", name, err)
		}
		if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(quotaFieldManagerName), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("apply ResourceRegistration %q: %w", name, err)
		}
		desired[name] = struct{}{}
	}
	return desired, nil
}

func (f *QuotaFanOut) applyClaimCreationPolicies(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
) (map[string]struct{}, error) {
	desired := make(map[string]struct{}, len(sc.Spec.Quota.MetricRules))
	for i := range sc.Spec.Quota.MetricRules {
		rule := &sc.Spec.Quota.MetricRules[i]

		// Resolve the preferred API version for the trigger kind.
		mapping, err := f.RESTMapper.RESTMapping(
			schema.GroupKind{Group: rule.Selector.APIGroup, Kind: rule.Selector.Kind},
		)
		if err != nil {
			if meta.IsNoMatchError(err) {
				return nil, fmt.Errorf("resolve REST mapping for %s/%s: %w", rule.Selector.APIGroup, rule.Selector.Kind, err)
			}
			return nil, fmt.Errorf("resolve REST mapping for %s/%s: %w", rule.Selector.APIGroup, rule.Selector.Kind, err)
		}
		apiVersion := mapping.GroupVersionKind.GroupVersion().String()

		name := encodeName(rule.Selector.APIGroup + "-" + rule.Selector.Kind)

		// Build ResourceRequests from MetricCosts.
		requests := make([]quotav1alpha1.ResourceRequest, 0, len(rule.MetricCosts))
		for metricName, amount := range rule.MetricCosts {
			requests = append(requests, quotav1alpha1.ResourceRequest{
				ResourceType: metricName,
				Amount:       amount,
			})
		}

		obj := &quotav1alpha1.ClaimCreationPolicy{
			TypeMeta: metav1.TypeMeta{
				APIVersion: quotav1alpha1.GroupVersion.String(),
				Kind:       "ClaimCreationPolicy",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					labelManagedBy: labelManagedByValue,
				},
			},
			Spec: quotav1alpha1.ClaimCreationPolicySpec{
				Trigger: quotav1alpha1.ClaimTriggerSpec{
					Resource: quotav1alpha1.ClaimTriggerResource{
						APIVersion: apiVersion,
						Kind:       rule.Selector.Kind,
					},
				},
				Target: quotav1alpha1.ClaimTargetSpec{
					ResourceClaimTemplate: quotav1alpha1.ResourceClaimTemplate{
						Metadata: quotav1alpha1.ObjectMetaTemplate{
							GenerateName: "{{trigger.metadata.name}}-quota-",
							Namespace:    "{{requestInfo.namespace}}",
						},
						Spec: quotav1alpha1.ResourceClaimSpec{
							Requests: requests,
						},
					},
				},
			},
		}
		if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
			return nil, fmt.Errorf("set controller ref on ClaimCreationPolicy %q: %w", name, err)
		}
		if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(quotaFieldManagerName), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("apply ClaimCreationPolicy %q: %w", name, err)
		}
		desired[name] = struct{}{}
	}
	return desired, nil
}

func (f *QuotaFanOut) pruneResourceRegistrations(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desired map[string]struct{},
) error {
	var list quotav1alpha1.ResourceRegistrationList
	if err := f.Client.List(ctx, &list, client.MatchingLabels{labelManagedBy: labelManagedByValue}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list ResourceRegistrations: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if !ownedBy(obj.OwnerReferences, sc.UID) {
			continue
		}
		if _, keep := desired[obj.Name]; keep {
			continue
		}
		if err := f.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ResourceRegistration %q: %w", obj.Name, err)
		}
	}
	return nil
}

func (f *QuotaFanOut) pruneClaimCreationPolicies(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desired map[string]struct{},
) error {
	var list quotav1alpha1.ClaimCreationPolicyList
	if err := f.Client.List(ctx, &list, client.MatchingLabels{labelManagedBy: labelManagedByValue}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list ClaimCreationPolicies: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if !ownedBy(obj.OwnerReferences, sc.UID) {
			continue
		}
		if _, keep := desired[obj.Name]; keep {
			continue
		}
		if err := f.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ClaimCreationPolicy %q: %w", obj.Name, err)
		}
	}
	return nil
}
