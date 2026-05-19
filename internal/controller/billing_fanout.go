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

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const (
	labelManagedBy      = "app.kubernetes.io/managed-by"
	labelManagedByValue = "services-operator"
	labelOwnerService   = "services.miloapis.com/service"
	fieldManagerName    = "services-operator"
)

// BillingFanOut materializes the downstream billing objects declared by
// a ServiceConfiguration (MeterDefinition, MonitoredResourceType) via
// server-side apply and prunes previously-applied objects that no longer
// appear in the desired set.
type BillingFanOut struct {
	Client client.Client
	Scheme *runtime.Scheme
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

		obj := &billingv1alpha1.MonitoredResourceType{
			TypeMeta: metav1.TypeMeta{
				APIVersion: billingv1alpha1.GroupVersion.String(),
				Kind:       "MonitoredResourceType",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					labelManagedBy:    labelManagedByValue,
					labelOwnerService: serviceName,
				},
			},
			Spec: billingv1alpha1.MonitoredResourceTypeSpec{
				ResourceTypeName: entry.Type,
				Phase:            billingv1alpha1.Phase(sc.Spec.Phase),
				DisplayName:      entry.DisplayName,
				Description:      entry.Description,
				GVK: billingv1alpha1.MonitoredResourceTypeGVK{
					Group: entry.GVK.Group,
					Kind:  entry.GVK.Kind,
				},
				Labels: billingLabelsFor(entry.Labels),
			},
		}
		if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
			return nil, fmt.Errorf("set controller ref on billing MonitoredResourceType %q: %w", name, err)
		}
		if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
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
		obj := &billingv1alpha1.MeterDefinition{
			TypeMeta: metav1.TypeMeta{
				APIVersion: billingv1alpha1.GroupVersion.String(),
				Kind:       "MeterDefinition",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					labelManagedBy:    labelManagedByValue,
					labelOwnerService: serviceName,
				},
			},
			Spec: billingv1alpha1.MeterDefinitionSpec{
				MeterName:   metric.Name,
				Phase:       billingv1alpha1.Phase(sc.Spec.Phase),
				DisplayName: metric.DisplayName,
				Description: metric.Description,
				Measurement: billingv1alpha1.MeterMeasurement{
					Aggregation: metricKindToAggregation(metric.Kind),
					Unit:        metric.Unit,
				},
				Billing: billingv1alpha1.MeterBilling{
					// Default both to the emission unit. The future SKU layer
					// will diverge these when pricing units differ.
					ConsumedUnit: metric.Unit,
					PricingUnit:  metric.Unit,
				},
				MonitoredResourceTypes: mrtTypes,
			},
		}
		if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
			return nil, fmt.Errorf("setting controller reference on MeterDefinition %q: %w", name, err)
		}
		if err := f.Client.Patch(ctx, obj, client.Apply,
			client.FieldOwner(fieldManagerName),
			client.ForceOwnership,
		); err != nil {
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
	if err := f.Client.List(ctx, &list, client.MatchingLabels{labelManagedBy: labelManagedByValue}); err != nil {
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
	if err := f.Client.List(ctx, &list, client.MatchingLabels{labelManagedBy: labelManagedByValue}); err != nil {
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


func billingLabelsFor(labels []servicesv1alpha1.MonitoredResourceLabel) []billingv1alpha1.MonitoredResourceLabel {
	if len(labels) == 0 {
		return nil
	}
	out := make([]billingv1alpha1.MonitoredResourceLabel, 0, len(labels))
	for _, l := range labels {
		out = append(out, billingv1alpha1.MonitoredResourceLabel{
			Name:        l.Name,
			Description: l.Description,
		})
	}
	return out
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}
