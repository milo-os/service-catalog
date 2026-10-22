// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// restClientLimiter returns the limiter client-go builds for cfg, which is
// what actually throttles requests, rather than trusting the config fields.
func restClientLimiter(t *testing.T, cfg *rest.Config) flowcontrol.RateLimiter {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	cfg.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	restClient, err := rest.UnversionedRESTClientFor(cfg)
	if err != nil {
		t.Fatalf("build REST client: %v", err)
	}
	return restClient.GetRateLimiter()
}

// A config loaded from kubeconfigPath has QPS and Burst unset, which client-go
// turns into 5 and 10. The defaults must replace that, in the config the
// project control-plane configs are copied from.
func TestSetClientRateLimits_DefaultsReplaceClientGoDefaults(t *testing.T) {
	cfg := &rest.Config{Host: "https://milo.example.com"}
	setClientRateLimits(cfg, defaultKubeAPIQPS, defaultKubeAPIBurst)

	limiter := restClientLimiter(t, rest.CopyConfig(cfg))
	if limiter == nil {
		t.Fatal("no rate limiter, want the default limits")
	}
	if limiter.QPS() != defaultKubeAPIQPS {
		t.Fatalf("limiter QPS = %v, want %v", limiter.QPS(), defaultKubeAPIQPS)
	}
	if defaultKubeAPIQPS < 5*rest.DefaultQPS || defaultKubeAPIBurst < 5*rest.DefaultBurst {
		t.Fatalf("defaults %v/%v are too close to client-go's %v/%v, which held setup for seconds",
			defaultKubeAPIQPS, defaultKubeAPIBurst, rest.DefaultQPS, rest.DefaultBurst)
	}
}

// A burst of the default size goes through without waiting: an activation's
// status writes fit inside it.
func TestSetClientRateLimits_DefaultBurstIsNotThrottled(t *testing.T) {
	cfg := &rest.Config{Host: "https://milo.example.com"}
	setClientRateLimits(cfg, defaultKubeAPIQPS, defaultKubeAPIBurst)
	limiter := restClientLimiter(t, cfg)
	for i := 0; i < defaultKubeAPIBurst; i++ {
		if !limiter.TryAccept() {
			t.Fatalf("request %d of a %d-request burst was throttled", i+1, defaultKubeAPIBurst)
		}
	}
}

func TestSetClientRateLimits_NegativeQPSDisablesThrottling(t *testing.T) {
	cfg := &rest.Config{Host: "https://milo.example.com"}
	setClientRateLimits(cfg, -1, 0)
	if limiter := restClientLimiter(t, cfg); limiter != nil {
		t.Fatalf("rate limiter %T (QPS %v), want none", limiter, limiter.QPS())
	}
}

func TestSetClientRateLimits_FlagValuesAreUsed(t *testing.T) {
	cfg := &rest.Config{Host: "https://milo.example.com"}
	setClientRateLimits(cfg, 7, 9)
	limiter := restClientLimiter(t, cfg)
	if limiter == nil || limiter.QPS() != 7 {
		t.Fatalf("limiter = %v, want QPS 7", limiter)
	}
}

func TestValidateClientRateLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		qps     float32
		burst   int
		wantErr bool
	}{
		{"defaults", defaultKubeAPIQPS, defaultKubeAPIBurst, false},
		{"throttling off", -1, 0, false},
		{"zero QPS falls back to client-go's default", 0, 100, true},
		{"zero burst falls back to client-go's default", 50, 0, true},
		{"negative burst", 50, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClientRateLimits(tc.qps, tc.burst)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateClientRateLimits(%v, %d) error = %v, wantErr %v", tc.qps, tc.burst, err, tc.wantErr)
			}
		})
	}
}
