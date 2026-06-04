// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Observability for the consumer provider. Metrics are registered with
// controller-runtime's global registry so they are exposed on the manager's
// existing /metrics endpoint without extra wiring by the adopter.
var (
	// engagedClusters tracks how many consumer projects are currently engaged.
	// It is a process-wide gauge (the provider is a singleton per operator), set
	// after each successful engage/disengage.
	engagedClusters = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "consumer_provider_engaged_clusters",
		Help: "Number of consumer projects currently engaged by the consumer provider.",
	})

	// teardownFailuresTotal counts deactivation-teardown failures, labelled by
	// consumer project. Teardown is retried with backoff, so this is alert-only:
	// a continuously climbing value for a project signals a teardown that cannot
	// converge (e.g. a forbidden delete) and needs operator attention. Cardinality
	// is bounded — only projects that actually fail teardown ever get a series.
	teardownFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "consumer_provider_teardown_failures_total",
		Help: "Total consumer-project teardown failures (alert-only; teardown is retried with backoff).",
	}, []string{"consumer_project"})
)

func init() {
	metrics.Registry.MustRegister(engagedClusters, teardownFailuresTotal)
}

// setEngagedClusters publishes the current engaged-cluster count.
func setEngagedClusters(n int) {
	engagedClusters.Set(float64(n))
}

// recordTeardownFailure increments the teardown-failure counter for a project.
func (p *Provider) recordTeardownFailure(consumerProject string) {
	teardownFailuresTotal.WithLabelValues(consumerProject).Inc()
}
