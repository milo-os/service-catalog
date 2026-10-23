// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func testServiceCatalog() map[string]*servicesv1alpha1.Service {
	compute := &servicesv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{Name: "compute-miloapis-com"}, Spec: servicesv1alpha1.ServiceSpec{ServiceName: "compute.miloapis.com"}}
	return map[string]*servicesv1alpha1.Service{compute.Name: compute, compute.Spec.ServiceName: compute}
}

func TestServiceMetricNameIsBoundedByCatalog(t *testing.T) {
	services := testServiceCatalog()
	for _, tc := range []struct {
		ref, statusName, want string
	}{
		{ref: "compute-miloapis-com", want: "compute.miloapis.com"},
		{ref: "renamed", statusName: "compute.miloapis.com", want: "compute.miloapis.com"},
		{ref: "typo-of-a-service", want: serviceLabelUnknown},
	} {
		entitlement := &servicesv1alpha1.ServiceEntitlement{}
		entitlement.Spec.ServiceRef.Name = tc.ref
		entitlement.Status.ServiceName = tc.statusName
		if got := serviceMetricName(services, entitlement); got != tc.want {
			t.Errorf("serviceMetricName(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
	if got := serviceLabel(&servicesv1alpha1.Service{}); got != serviceLabelUnknown {
		t.Errorf("serviceLabel(empty Service) = %q, want %q", got, serviceLabelUnknown)
	}
}

func TestServiceUsageCollectorCountsByServiceWithoutProjects(t *testing.T) {
	services := testServiceCatalog()
	now := time.Now()
	entitlement := func(phase servicesv1alpha1.EntitlementPhase, origin servicesv1alpha1.EntitlementOrigin) servicesv1alpha1.ServiceEntitlement {
		e := servicesv1alpha1.ServiceEntitlement{}
		e.Spec.ServiceRef.Name = "compute-miloapis-com"
		e.Status.Phase = phase
		e.Status.Origin = origin
		return e
	}
	active := entitlement(servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.EntitlementOriginDirect)
	stalled := entitlement(servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.EntitlementOriginDependency)
	stalled.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{
		RequestedAt:  metav1.NewTime(now.Add(-2 * time.Hour)),
		AuthorizedAt: &metav1.Time{Time: now.Add(-2 * time.Hour)},
	}
	pending := entitlement("", "")
	deleting := entitlement(servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.EntitlementOriginDirect)
	deleting.DeletionTimestamp = &metav1.Time{Time: now}

	collector := newServiceUsageCollector()
	collector.replaceProject("a", []servicesv1alpha1.ServiceEntitlement{active, stalled, deleting}, services)
	collector.replaceProject("b", []servicesv1alpha1.ServiceEntitlement{active, pending}, services)

	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			key := family.GetName()
			for _, label := range metric.Label {
				if label.GetName() == "project" {
					t.Fatalf("%s carries a project label", family.GetName())
				}
				key += "," + label.GetName() + "=" + label.GetValue()
			}
			got[key] = metric.GetGauge().GetValue()
		}
	}
	want := map[string]float64{
		"service_catalog_service_entitlements,origin=Direct,phase=Active,service=compute.miloapis.com":     2,
		"service_catalog_service_entitlements,origin=Dependency,phase=Active,service=compute.miloapis.com": 1,
		"service_catalog_service_entitlements,origin=Unknown,phase=Pending,service=compute.miloapis.com":   1,
		"service_catalog_service_setup_inflight,blocking_stage=Authorization,service=compute.miloapis.com": 1,
		"service_catalog_service_setup_stalled,service=compute.miloapis.com":                               1,
	}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v (all: %v)", key, got[key], value, got)
		}
	}

	collector.removeProject("a")
	collector.removeProject("b")
	if families, err := registry.Gather(); err != nil || len(families) != 0 {
		t.Fatalf("removed projects still report series: %v (err %v)", families, err)
	}
}

func TestServiceRejectionCountsOnlyTheTransition(t *testing.T) {
	before := &servicesv1alpha1.ServiceEntitlement{}
	after := before.DeepCopy()
	after.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	after.Status.ServiceName = "rejection-test.miloapis.com"
	counter := serviceActivations.WithLabelValues("rejection-test.miloapis.com", serviceActivationRejected)
	start := counterValue(t, counter)

	observeServiceRejection(before, after)
	observeServiceRejection(after, after)

	if got := counterValue(t, counter) - start; got != 1 {
		t.Fatalf("rejections counted = %v, want 1", got)
	}
}

func TestServiceDisableFallsBackToRecordedServiceName(t *testing.T) {
	entitlement := &servicesv1alpha1.ServiceEntitlement{}
	entitlement.Status.ServiceName = "disable-test.miloapis.com"
	entitlement.Status.Origin = servicesv1alpha1.EntitlementOriginDirect
	counter := serviceDisables.WithLabelValues("disable-test.miloapis.com", "Direct")
	start := counterValue(t, counter)

	// The finalizer substitutes an empty Service when the Service is gone.
	observeServiceDisable(&servicesv1alpha1.Service{}, entitlement)

	if got := counterValue(t, counter) - start; got != 1 {
		t.Fatalf("disables counted = %v, want 1", got)
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetCounter().GetValue()
}
