// SPDX-License-Identifier: AGPL-3.0-only

package controller

import "testing"

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
