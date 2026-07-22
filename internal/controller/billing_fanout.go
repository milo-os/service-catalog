// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	billingapply "go.miloapis.com/billing/applyconfiguration/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	labelManagedBy      = "app.kubernetes.io/managed-by"
	labelManagedByValue = "services.miloapis.com"
	labelOwnerService   = "services.miloapis.com/service"
	fieldManagerName    = "services-operator"

	// labelManagedByValueLegacy is the previous managed-by value. It is matched
	// (in addition to labelManagedByValue) when pruning fan-out objects so that
	// objects created by an earlier release are not stranded after the rename.
	// Drop this once all clusters have reconciled past the change.
	labelManagedByValueLegacy = "services-operator"
)

// managedByFanoutSelector matches fan-out objects carrying either the current
// or the legacy managed-by value. Used by prune logic during the transitional
// release so a value rename does not orphan previously-created objects.
var managedByFanoutSelector = labels.NewSelector().Add(
	mustLabelRequirement(labelManagedBy, selection.In,
		labelManagedByValue, labelManagedByValueLegacy),
)

func mustLabelRequirement(key string, op selection.Operator, vals ...string) labels.Requirement {
	req, err := labels.NewRequirement(key, op, vals)
	if err != nil {
		panic(fmt.Sprintf("invalid label requirement %s %s %v: %v", key, op, vals, err))
	}
	return *req
}

// BillingFanOut materializes the downstream billing objects declared by
// a ServiceConfiguration (MeterDefinition, MonitoredResourceType) via
// server-side apply and prunes previously-applied objects that no longer
// appear in the desired set.
type BillingFanOut struct {
	Client client.Client
}

// Reconcile applies every billing object declared by sc and deletes any
// previously-managed billing object owned by sc that is no longer in the
// desired set. Draft configurations are skipped; previously-applied
// objects remain in place until sc transitions past Draft.
func (f *BillingFanOut) Reconcile(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		return nil
	}

	serviceName, err := f.resolveServiceName(ctx, sc)
	if err != nil {
		return err
	}

	desiredMRTs, err := f.applyMonitoredResourceTypes(ctx, sc, serviceName)
	if err != nil {
		return err
	}
	desiredMeters, err := f.applyMeterDefinitions(ctx, sc, serviceName)
	if err != nil {
		return err
	}

	if err := f.pruneMonitoredResourceTypes(ctx, sc, desiredMRTs); err != nil {
		return err
	}
	return f.pruneMeters(ctx, sc, desiredMeters)
}

// Cleanup deletes every billing object owned by sc. Used during
// finalization to release managed state before the owner record goes
// away.
func (f *BillingFanOut) Cleanup(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if err := f.pruneMonitoredResourceTypes(ctx, sc, nil); err != nil {
		return err
	}
	return f.pruneMeters(ctx, sc, nil)
}

func (f *BillingFanOut) resolveServiceName(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) (string, error) {
	var svc servicesv1alpha1.Service
	if err := f.Client.Get(ctx, client.ObjectKey{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		return "", fmt.Errorf("resolve Service %q: %w", sc.Spec.ServiceRef.Name, err)
	}
	return svc.Spec.ServiceName, nil
}

func (f *BillingFanOut) applyMonitoredResourceTypes(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName string,
) (map[string]struct{}, error) {
	desired := make(map[string]struct{}, len(sc.Spec.MonitoredResourceTypes))
	for i := range sc.Spec.MonitoredResourceTypes {
		entry := &sc.Spec.MonitoredResourceTypes[i]
		name := encodeName(entry.Type)

		applyConfig := billingapply.MonitoredResourceType(name, "").
			WithLabels(map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: serviceName,
			}).
			WithOwnerReferences(controllerRef(sc)).
			WithSpec(billingapply.MonitoredResourceTypeSpec().
				WithResourceTypeName(entry.Type).
				WithPhase(billingv1alpha1.Phase(sc.Spec.Phase)).
				WithDisplayName(entry.DisplayName).
				WithDescription(entry.Description).
				WithGVK(billingapply.MonitoredResourceTypeGVK().
					WithGroup(entry.GVK.Group).
					WithKind(entry.GVK.Kind),
				).
				WithLabels(billingLabelsApplyFor(entry.Labels)...),
			)
		if err := f.Client.Apply(ctx, applyConfig, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("apply billing MonitoredResourceType %q: %w", name, err)
		}
		desired[name] = struct{}{}
	}
	return desired, nil
}

// buildMetricToMRTsIndex inverts spec.billing.consumerDestinations into a map
// of metric name → slice of monitored resource type names.
func buildMetricToMRTsIndex(sc *servicesv1alpha1.ServiceConfiguration) map[string][]string {
	idx := make(map[string][]string)
	if sc.Spec.Billing == nil {
		return idx
	}
	for _, dest := range sc.Spec.Billing.ConsumerDestinations {
		for _, metricName := range dest.Metrics {
			idx[metricName] = append(idx[metricName], dest.MonitoredResourceType)
		}
	}
	return idx
}

// metricKindToAggregation maps a MetricKind to its billing aggregation.
// Delta and Cumulative both aggregate as Sum; Gauge aggregates as Latest.
func metricKindToAggregation(kind servicesv1alpha1.MetricKind) billingv1alpha1.MeterAggregation {
	if kind == servicesv1alpha1.MetricKindGauge {
		return billingv1alpha1.MeterAggregationLatest
	}
	return billingv1alpha1.MeterAggregationSum
}

func (f *BillingFanOut) applyMeterDefinitions(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName string,
) (map[string]struct{}, error) {
	metricToMRTs := buildMetricToMRTsIndex(sc)
	desired := make(map[string]struct{}, len(sc.Spec.Metrics))

	for i := range sc.Spec.Metrics {
		metric := &sc.Spec.Metrics[i]
		mrtTypes, hasDest := metricToMRTs[metric.Name]
		if !hasDest || len(mrtTypes) == 0 {
			// Quota-only metrics with no billing destination produce no
			// MeterDefinition — MonitoredResourceTypes requires MinItems=1.
			continue
		}

		name := encodeName(metric.Name)
		applyConfig := billingapply.MeterDefinition(name, "").
			WithLabels(map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: serviceName,
			}).
			WithOwnerReferences(controllerRef(sc)).
			WithSpec(billingapply.MeterDefinitionSpec().
				WithMeterName(metric.Name).
				WithPhase(billingv1alpha1.Phase(sc.Spec.Phase)).
				WithDisplayName(metric.DisplayName).
				WithDescription(metric.Description).
				WithMeasurement(billingapply.MeterMeasurement().
					WithAggregation(metricKindToAggregation(metric.Kind)).
					WithUnit(metric.Unit).
					WithDimensions(metric.Dimensions...),
				).
				WithBilling(billingapply.MeterBilling().
					// Default both to the emission unit. The future SKU layer
					// will diverge these when pricing units differ.
					WithConsumedUnit(metric.Unit).
					WithPricingUnit(metric.Unit),
				).
				WithMonitoredResourceTypes(mrtTypes...),
			)
		if err := f.Client.Apply(ctx, applyConfig, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("applying MeterDefinition %q: %w", name, err)
		}
		desired[name] = struct{}{}
	}
	return desired, nil
}

func (f *BillingFanOut) pruneMonitoredResourceTypes(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desired map[string]struct{},
) error {
	var list billingv1alpha1.MonitoredResourceTypeList
	if err := f.Client.List(ctx, &list, client.MatchingLabelsSelector{Selector: managedByFanoutSelector}); err != nil {
		return fmt.Errorf("list billing MonitoredResourceTypes: %w", err)
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
			return fmt.Errorf("delete stale billing MonitoredResourceType %q: %w", obj.Name, err)
		}
	}
	return nil
}

func (f *BillingFanOut) pruneMeters(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desired map[string]struct{},
) error {
	var list billingv1alpha1.MeterDefinitionList
	if err := f.Client.List(ctx, &list, client.MatchingLabelsSelector{Selector: managedByFanoutSelector}); err != nil {
		return fmt.Errorf("list billing MeterDefinitions: %w", err)
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
			return fmt.Errorf("delete stale billing MeterDefinition %q: %w", obj.Name, err)
		}
	}
	return nil
}

func billingLabelsApplyFor(labels []servicesv1alpha1.MonitoredResourceLabel) []*billingapply.MonitoredResourceLabelApplyConfiguration {
	out := make([]*billingapply.MonitoredResourceLabelApplyConfiguration, 0, len(labels))
	for _, l := range labels {
		out = append(out, billingapply.MonitoredResourceLabel().
			WithName(l.Name).
			WithDescription(l.Description),
		)
	}
	return out
}

// controllerRef builds an owner reference that marks sc as the controller of a
// fan-out billing object. Replaces ctrl.SetControllerReference for use with
// apply configuration types, which don't implement runtime.Object.
func controllerRef(sc *servicesv1alpha1.ServiceConfiguration) *metav1apply.OwnerReferenceApplyConfiguration {
	t := true
	return metav1apply.OwnerReference().
		WithAPIVersion(servicesv1alpha1.GroupVersion.String()).
		WithKind("ServiceConfiguration").
		WithName(sc.Name).
		WithUID(sc.UID).
		WithController(t).
		WithBlockOwnerDeletion(t)
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}
