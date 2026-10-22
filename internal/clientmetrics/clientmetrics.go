// SPDX-License-Identifier: AGPL-3.0-only

// Package clientmetrics reports how long this operator's API requests take and
// how long they wait on client-side rate limiting.
//
// controller-runtime registers only client-go's request counter. Without
// latency, a slow setup cannot be traced to a slow API server versus the
// operator's own throttling. client-go accepts its metric hooks once, and
// controller-runtime has already claimed that, so this package assigns the
// latency hooks directly instead of calling metrics.Register.
package clientmetrics

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// scopeRoot is a request to the platform's root API.
	scopeRoot = "root"
	// scopeProject is a request to one project's control plane. Projects are
	// not labelled individually, to keep the series count independent of how
	// many projects exist.
	scopeProject = "project"
)

// buckets match client-go's own rest_client_request_duration_seconds buckets.
var buckets = []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60}

var (
	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_request_duration_seconds",
		Help:    "Latency of API requests made by the operator, by verb, host and scope (root or project).",
		Buckets: buckets,
	}, []string{"verb", "host", "scope"})

	rateLimiterDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_rate_limiter_duration_seconds",
		Help:    "Time API requests waited on the operator's client-side rate limiter, by verb, host and scope.",
		Buckets: buckets,
	}, []string{"verb", "host", "scope"})

	registerOnce sync.Once
)

// Register installs the latency hooks and registers their collectors with
// controller-runtime's registry. It is safe to call more than once.
func Register() {
	registerOnce.Do(func() {
		crmetrics.Registry.MustRegister(requestDuration, rateLimiterDuration)
		clientmetrics.RequestLatency = latencyAdapter{metric: requestDuration}
		clientmetrics.RateLimiterLatency = latencyAdapter{metric: rateLimiterDuration}
	})
}

type latencyAdapter struct {
	metric *prometheus.HistogramVec
}

func (a latencyAdapter) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	a.metric.WithLabelValues(verb, u.Host, scopeOf(u.Path)).Observe(latency.Seconds())
}

// scopeOf reports whether a request targets a project control plane, which
// Milo serves under /apis/resourcemanager.miloapis.com/<version>/projects/<name>/control-plane.
func scopeOf(path string) string {
	if strings.Contains(path, "/projects/") && strings.Contains(path, "/control-plane") {
		return scopeProject
	}
	return scopeRoot
}
