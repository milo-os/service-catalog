// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestSetupMetricSeriesAreInitialized(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(setupDuration, setupAttempts)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int, 2)
	for _, family := range families {
		counts[family.GetName()] = len(family.Metric)
	}
	if got, want := counts["service_catalog_setup_duration_seconds"], len(setupServiceClasses)*len(setupOperations)*len(setupClocks); got != want {
		t.Fatalf("initialized setup duration label combinations = %d, want %d", got, want)
	}
	if got, want := counts["service_catalog_setup_attempts_total"], len(setupServiceClasses)*len(setupOperations)*len(setupOutcomes); got != want {
		t.Fatalf("initialized setup attempt label combinations = %d, want %d", got, want)
	}
}

func TestSetupInflightCollectorRemovesProject(t *testing.T) {
	collector := newSetupInflightCollector()
	collector.projects["removed"] = []setupInflightEntry{{class: "compute", operation: "initial", blocker: "Projection", requestedAt: time.Now()}}
	collector.projects["retained"] = []setupInflightEntry{{class: "networking", operation: "initial", blocker: "Dependencies", requestedAt: time.Now()}}

	collector.removeProject("removed")

	if _, ok := collector.projects["removed"]; ok {
		t.Fatal("removed project still contributes setup in-flight metrics")
	}
	if _, ok := collector.projects["retained"]; !ok {
		t.Fatal("removing one project deleted another project's setup metrics")
	}
}

func TestSetupDurationBucketsCoverLongRunningSetup(t *testing.T) {
	want := []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, 86400}
	if len(setupDurationBuckets) != len(want) {
		t.Fatalf("setup duration buckets = %v, want %v", setupDurationBuckets, want)
	}
	for i := range want {
		if setupDurationBuckets[i] != want[i] {
			t.Fatalf("setup duration buckets = %v, want %v", setupDurationBuckets, want)
		}
	}
}

func TestSetupMetricSeriesBudget(t *testing.T) {
	if got := setupMetricSeriesBudget(); got != 377 {
		t.Fatalf("setup metrics add %d series, want 377", got)
	} else if got >= 20_000 {
		t.Fatalf("setup metrics add %d series, want fewer than 20,000", got)
	}
}

func TestSetupServiceClassIsBounded(t *testing.T) {
	allowed := map[string]bool{"compute": true, "networking": true, "dns": true, "storage": true, "other": true}
	for _, service := range []string{
		"compute.miloapis.com", "networking.datumapis.com", "dns.example", "storage.example",
		"customer-created-service-123456789",
	} {
		if got := setupServiceClass(service); !allowed[got] {
			t.Fatalf("setupServiceClass(%q) = %q, outside bounded vocabulary", service, got)
		}
	}
}

func TestSetupInflightExcludesUnfinishableAttemptsAndApprovalWaits(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	attempt := func(name string, mutate func(*servicesv1alpha1.ServiceEntitlement)) servicesv1alpha1.ServiceEntitlement {
		e := servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute.miloapis.com"}},
			Status: servicesv1alpha1.ServiceEntitlementStatus{
				Phase: servicesv1alpha1.EntitlementPhaseActive,
				Setup: &servicesv1alpha1.ServiceSetupStatus{RequestedAt: old},
			},
		}
		mutate(&e)
		return e
	}
	entitlements := []servicesv1alpha1.ServiceEntitlement{
		attempt("rejected", func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
		}),
		attempt("deleting", func(e *servicesv1alpha1.ServiceEntitlement) { e.DeletionTimestamp = &old }),
		attempt("backfilled", func(e *servicesv1alpha1.ServiceEntitlement) { e.Status.Setup.Backfilled = true }),
		attempt("awaiting-approval", func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Phase = servicesv1alpha1.EntitlementPhasePendingApproval
		}),
		attempt("authorized", func(e *servicesv1alpha1.ServiceEntitlement) { e.Status.Setup.AuthorizedAt = &old }),
	}

	collector := newSetupInflightCollector()
	collector.replaceProject("project", entitlements)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	totals := make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			totals[family.GetName()] += metric.GetGauge().GetValue()
		}
	}
	if got := totals["service_catalog_setup_inflight"]; got != 2 {
		t.Errorf("in-flight setups = %v, want 2 (awaiting approval and authorized)", got)
	}
	if got := totals["service_catalog_setup_overdue"]; got != 1 {
		t.Errorf("overdue setups = %v, want 1 (approval waits are not overdue)", got)
	}
}
