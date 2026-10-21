// SPDX-License-Identifier: AGPL-3.0-only

package controller

import "testing"

func TestSetupMetricSeriesBudget(t *testing.T) {
	if got := setupMetricSeriesBudget(); got >= 20_000 {
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
