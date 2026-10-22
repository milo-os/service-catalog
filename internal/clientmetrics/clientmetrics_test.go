// SPDX-License-Identifier: AGPL-3.0-only

package clientmetrics

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/client-go/rest"
	clientmetrics "k8s.io/client-go/tools/metrics"
	"k8s.io/client-go/util/flowcontrol"
)

func TestRegisterReportsLatencyByScope(t *testing.T) {
	Register()
	Register() // a second call must not panic on duplicate registration

	project := url.URL{Host: "milo-apiserver:6443",
		Path: "/apis/resourcemanager.miloapis.com/v1alpha1/projects/p1/control-plane/apis/services.miloapis.com/v1alpha1/serviceentitlements"}
	root := url.URL{Host: "milo-apiserver:6443", Path: "/apis/services.miloapis.com/v1alpha1/serviceconfigurations"}

	clientmetrics.RequestLatency.Observe(context.Background(), "PATCH", project, 300*time.Millisecond)
	clientmetrics.RequestLatency.Observe(context.Background(), "GET", root, 10*time.Millisecond)
	clientmetrics.RateLimiterLatency.Observe(context.Background(), "PATCH", project, 2*time.Second)

	if got := countSeries(requestDuration); got != 2 {
		t.Errorf("request duration series = %d, want 2 (one per verb, host and scope)", got)
	}
	if got := countSeries(rateLimiterDuration); got != 1 {
		t.Errorf("rate limiter series = %d, want 1", got)
	}
}

func TestRegisterCountsRetries(t *testing.T) {
	Register()
	before := value(t, requestRetries.WithLabelValues("429", "PATCH", "milo-apiserver:6443"))
	clientmetrics.RequestRetry.IncrementRetry(context.Background(), "429", "PATCH", "milo-apiserver:6443")
	if got := value(t, requestRetries.WithLabelValues("429", "PATCH", "milo-apiserver:6443")) - before; got != 1 {
		t.Errorf("429 retries = %v, want 1", got)
	}
}

func TestReportRateLimitResolvesClientGoDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg        rest.Config
		qps, burst float64
	}{
		// A kubeconfig loaded with clientcmd leaves both at 0, which client-go
		// turns into its 5 QPS / burst 10 limiter.
		"unset":       {cfg: rest.Config{}, qps: 5, burst: 10},
		"explicit":    {cfg: rest.Config{QPS: 50, Burst: 100}, qps: 50, burst: 100},
		"disabled":    {cfg: rest.Config{QPS: -1}, qps: 0, burst: 0},
		"custom":      {cfg: rest.Config{RateLimiter: flowcontrol.NewTokenBucketRateLimiter(20, 40), Burst: 40}, qps: 20, burst: 40},
		"burst-unset": {cfg: rest.Config{QPS: 30}, qps: 30, burst: 10},
	} {
		t.Run(name, func(t *testing.T) {
			ReportRateLimit(&tc.cfg)
			if got := value(t, rateLimit.WithLabelValues("qps")); got != tc.qps {
				t.Errorf("qps = %v, want %v", got, tc.qps)
			}
			if got := value(t, rateLimit.WithLabelValues("burst")); got != tc.burst {
				t.Errorf("burst = %v, want %v", got, tc.burst)
			}
		})
	}
}

func TestScopeDoesNotLabelIndividualProjects(t *testing.T) {
	for path, want := range map[string]string{
		"/apis/resourcemanager.miloapis.com/v1alpha1/projects/a/control-plane/api/v1/namespaces": scopeProject,
		"/apis/resourcemanager.miloapis.com/v1alpha1/projects/b/control-plane/apis/x/v1/ys":      scopeProject,
		"/apis/resourcemanager.miloapis.com/v1alpha1/projects/a":                                 scopeRoot,
		"/apis/services.miloapis.com/v1alpha1/services":                                          scopeRoot,
	} {
		if got := scopeOf(path); got != want {
			t.Errorf("scopeOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func countSeries(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)
	n := 0
	for range ch {
		n++
	}
	return n
}

func value(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var out dto.Metric
	if err := m.Write(&out); err != nil {
		t.Fatalf("read metric: %v", err)
	}
	if out.Counter != nil {
		return out.Counter.GetValue()
	}
	return out.Gauge.GetValue()
}
