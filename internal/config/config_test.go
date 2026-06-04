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
