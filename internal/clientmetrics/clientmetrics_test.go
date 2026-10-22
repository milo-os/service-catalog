// SPDX-License-Identifier: AGPL-3.0-only

package clientmetrics

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
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
