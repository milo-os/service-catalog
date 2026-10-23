// SPDX-License-Identifier: AGPL-3.0-only

// Package clientmetrics reports how long this operator's API requests take, how
// long they wait on client-side rate limiting, which requests the client
// retried, and the rate limit the operator's clients are built with.
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
	"k8s.io/client-go/rest"
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

	requestRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rest_client_request_retries_total",
		Help: "Retry attempts made by the operator's API clients, by the status code the retry attempt returned, verb and host. The first attempt is not counted; client-go retries after a 429 or a Retry-After response.",
	}, []string{"code", "verb", "host"})

	// rateLimit is the client-side limit every client built from the
	// operator's rest config enforces. 0 means the clients have no limiter.
	rateLimit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "service_catalog_client_rate_limit",
		Help: "Client-side API rate limit of each client the operator builds: limit=\"qps\" is requests per second, limit=\"burst\" is the burst size. 0 means the client is not rate limited.",
	}, []string{"limit"})

	registerOnce sync.Once
)

// Register installs the latency hooks and registers their collectors with
// controller-runtime's registry. It is safe to call more than once.
func Register() {
	registerOnce.Do(func() {
		crmetrics.Registry.MustRegister(requestDuration, rateLimiterDuration, requestRetries, rateLimit)
		clientmetrics.RequestLatency = latencyAdapter{metric: requestDuration}
		clientmetrics.RateLimiterLatency = latencyAdapter{metric: rateLimiterDuration}
		clientmetrics.RequestRetry = retryAdapter{metric: requestRetries}
	})
}

// ReportRateLimit records the client-side rate limit that client-go applies to
// every client built from cfg, resolving client-go's defaults: a QPS or burst
// of 0 means 5 requests per second with a burst of 10, and a negative QPS
// means no limiter. Without this, a dashboard cannot tell an unthrottled
// operator from one that does not export throttle metrics.
func ReportRateLimit(cfg *rest.Config) {
	qps, burst := effectiveRateLimit(cfg)
	rateLimit.WithLabelValues("qps").Set(qps)
	rateLimit.WithLabelValues("burst").Set(burst)
}

func effectiveRateLimit(cfg *rest.Config) (qps, burst float64) {
	if cfg.RateLimiter != nil {
		return float64(cfg.RateLimiter.QPS()), float64(cfg.Burst)
	}
	q, b := cfg.QPS, cfg.Burst
	if q == 0 {
		q = rest.DefaultQPS
	}
	if b == 0 {
		b = rest.DefaultBurst
	}
	if q < 0 {
		return 0, 0
	}
	return float64(q), float64(b)
}

type retryAdapter struct {
	metric *prometheus.CounterVec
}

func (a retryAdapter) IncrementRetry(_ context.Context, code, verb, host string) {
	a.metric.WithLabelValues(code, verb, host).Inc()
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
