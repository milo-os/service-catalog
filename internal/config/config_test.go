// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// decodeConfig decodes a ServicesOperator YAML doc through the SAME path
// cmd/services/main.go uses (strict universal decoder over the config scheme),
// so these tests guard the real decode behavior, not a hand-rolled unmarshal.
func decodeConfig(t *testing.T, yaml string) *ServicesOperator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add config scheme: %v", err)
	}
	if err := RegisterDefaults(scheme); err != nil {
		t.Fatalf("register defaults: %v", err)
	}
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)
	var cfg ServicesOperator
	if err := runtime.DecodeInto(codecs.UniversalDecoder(), []byte(yaml), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return &cfg
}

// tryDecodeConfig is the non-fatal variant of decodeConfig: it returns the
// decode error instead of failing, for negative (strict-decoding) tests.
func tryDecodeConfig(yaml string) error {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		return err
	}
	if err := RegisterDefaults(scheme); err != nil {
		return err
	}
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)
	var cfg ServicesOperator
	return runtime.DecodeInto(codecs.UniversalDecoder(), []byte(yaml), &cfg)
}

const baseConfigYAML = `apiVersion: apiserver.config.miloapis.com/v1alpha1
kind: ServicesOperator
metricsServer:
  bindAddress: "0"
`

// The gate defaults OFF: a config doc WITHOUT a consumerScopedProjection block
// decodes to a nil pointer, which main.go branches on to keep today's
// LocationBinding projection on the all-projects manager — byte-for-byte
// unchanged behavior.
func TestServicesOperator_ConsumerScopedProjectionAbsent_GateOff(t *testing.T) {
	cfg := decodeConfig(t, baseConfigYAML)
	if cfg.ConsumerScopedProjection != nil {
		t.Errorf("absent consumerScopedProjection must decode to nil (gate off), got %+v", cfg.ConsumerScopedProjection)
	}
}

// When the block IS present, it decodes to a populated config (gate on): the
// provider project, the canonical service names, and an optional resync override
// that parses as a metav1.Duration.
func TestServicesOperator_ConsumerScopedProjectionPresent_GateOn(t *testing.T) {
	yaml := baseConfigYAML + `consumerScopedProjection:
  providerProject: my-provider
  serviceNames:
  - compute.miloapis.com
  - storage.miloapis.com
  resyncInterval: 10m
`
	cfg := decodeConfig(t, yaml)
	csp := cfg.ConsumerScopedProjection
	if csp == nil {
		t.Fatalf("present consumerScopedProjection must decode to non-nil (gate on)")
	}
	if csp.ProviderProject != "my-provider" {
		t.Errorf("providerProject = %q, want %q", csp.ProviderProject, "my-provider")
	}
	want := []string{"compute.miloapis.com", "storage.miloapis.com"}
	if len(csp.ServiceNames) != len(want) || csp.ServiceNames[0] != want[0] || csp.ServiceNames[1] != want[1] {
		t.Errorf("serviceNames = %v, want %v", csp.ServiceNames, want)
	}
	if csp.ResyncInterval == nil {
		t.Fatalf("resyncInterval should parse to a non-nil metav1.Duration")
	}
	if csp.ResyncInterval.Duration != 10*time.Minute {
		t.Errorf("resyncInterval = %v, want 10m", csp.ResyncInterval.Duration)
	}
}

// resyncInterval is optional: when omitted the pointer stays nil so the provider
// applies its own 5m default rather than zero.
func TestServicesOperator_ConsumerScopedProjection_ResyncIntervalOmitted(t *testing.T) {
	yaml := baseConfigYAML + `consumerScopedProjection:
  providerProject: my-provider
  serviceNames:
  - compute.miloapis.com
`
	cfg := decodeConfig(t, yaml)
	if cfg.ConsumerScopedProjection == nil {
		t.Fatalf("consumerScopedProjection should be non-nil")
	}
	if cfg.ConsumerScopedProjection.ResyncInterval != nil {
		t.Errorf("omitted resyncInterval must stay nil, got %v", cfg.ConsumerScopedProjection.ResyncInterval)
	}
}

// Strict decoding (EnableStrict, as in main.go) rejects an unknown field inside
// the gate block. This guards against a silent typo — e.g. "serviceName"
// (singular) — leaving ServiceNames empty and quietly disabling the operator's
// consumer matching instead of failing fast at startup.
func TestServicesOperator_StrictDecodeRejectsTypoInGateBlock(t *testing.T) {
	yaml := baseConfigYAML + `consumerScopedProjection:
  providerProject: my-provider
  serviceName:
  - compute.miloapis.com
`
	if err := tryDecodeConfig(yaml); err == nil {
		t.Errorf("strict decoding must reject an unknown field (serviceName) in the gate block")
	}
}

// A config doc that says nothing about locations reads them from the group
// control planes serve today, so an existing deployment keeps its behavior.
func TestServicesOperator_LocationSourceDefaultsToNetworkServices(t *testing.T) {
	cfg := decodeConfig(t, baseConfigYAML)
	if cfg.LocationSource != LocationSourceNetworkServices {
		t.Errorf("locationSource = %q, want %q", cfg.LocationSource, LocationSourceNetworkServices)
	}
	gvk, err := cfg.LocationSource.GVK()
	if err != nil {
		t.Fatalf("resolve default location source: %v", err)
	}
	if gvk.Group != "networking.datumapis.com" || gvk.Version != "v1alpha" || gvk.Kind != "Location" {
		t.Errorf("default location source GVK = %v", gvk)
	}
}

func TestServicesOperator_LocationSourceLocationsService(t *testing.T) {
	cfg := decodeConfig(t, baseConfigYAML+"locationSource: locations.miloapis.com/v1alpha1\n")
	if cfg.LocationSource != LocationSourceLocationsService {
		t.Fatalf("locationSource = %q, want %q", cfg.LocationSource, LocationSourceLocationsService)
	}
	gvk, err := cfg.LocationSource.GVK()
	if err != nil {
		t.Fatalf("resolve location source: %v", err)
	}
	if gvk.Group != "locations.miloapis.com" || gvk.Version != "v1alpha1" || gvk.Kind != "Location" {
		t.Errorf("location source GVK = %v", gvk)
	}
}

// A source naming no known group is rejected, so the manager fails at startup
// rather than on the first reconcile that needs a location.
func TestServicesOperator_LocationSourceUnknownRejected(t *testing.T) {
	cfg := decodeConfig(t, baseConfigYAML+"locationSource: locations.example.com/v1\n")
	if _, err := cfg.LocationSource.GVK(); err == nil {
		t.Fatalf("expected an unknown location source to be rejected")
	}
}
