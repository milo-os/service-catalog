// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	setupDuration            = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "service_catalog_setup_duration_seconds", Help: "Elapsed time from the original accepted request to successful catalog setup.", Buckets: []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 1800, 3600}}, []string{"service_class", "operation", "clock"})
	setupAttempts            = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "service_catalog_setup_attempts_total", Help: "Terminal service setup attempts observed by the aggregate writer."}, []string{"service_class", "operation", "outcome"})
	projectionPhaseDuration  = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "service_catalog_projection_phase_duration_seconds", Help: "Time spent in bounded projection reconciliation phases.", Buckets: prometheus.ExponentialBuckets(0.005, 2, 12)}, []string{"phase"})
	setupObserverLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{Name: "service_catalog_setup_observer_last_success_timestamp_seconds", Help: "Unix timestamp of the last successful setup aggregation pass."})
	setupObserverHealthy     = prometheus.NewGauge(prometheus.GaugeOpts{Name: "service_catalog_setup_observer_healthy", Help: "Whether required setup source caches are synchronized on this projection owner."})
	setupInflight            = newSetupInflightCollector()
)

func init() {
	crmetrics.Registry.MustRegister(setupDuration, setupAttempts, projectionPhaseDuration, setupObserverLastSuccess, setupObserverHealthy, setupInflight)
}

type setupInflightEntry struct {
	class, operation, blocker string
	requestedAt               time.Time
}

type setupInflightCollector struct {
	mu                        sync.RWMutex
	projects                  map[string][]setupInflightEntry
	inflight, oldest, overdue *prometheus.Desc
}

func newSetupInflightCollector() *setupInflightCollector {
	labels := []string{"service_class", "operation", "blocking_stage"}
	return &setupInflightCollector{
		projects: make(map[string][]setupInflightEntry),
		inflight: prometheus.NewDesc("service_catalog_setup_inflight", "Unfinished persisted setup attempts.", labels, nil),
		oldest:   prometheus.NewDesc("service_catalog_setup_oldest_inflight_seconds", "Age of the oldest unfinished persisted setup attempt.", labels, nil),
		overdue:  prometheus.NewDesc("service_catalog_setup_overdue", "Unfinished setup attempts older than the initial 60 second objective.", labels, nil),
	}
}

func (c *setupInflightCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.inflight
	ch <- c.oldest
	ch <- c.overdue
}

func (c *setupInflightCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	type aggregate struct {
		count, overdue int
		oldest         float64
	}
	values := make(map[[3]string]aggregate)
	now := time.Now()
	for _, entries := range c.projects {
		for _, entry := range entries {
			key, value := [3]string{entry.class, entry.operation, entry.blocker}, values[[3]string{entry.class, entry.operation, entry.blocker}]
			age := now.Sub(entry.requestedAt).Seconds()
			if age < 0 {
				age = 0
			}
			value.count++
			if age > value.oldest {
				value.oldest = age
			}
			if age > 60 {
				value.overdue++
			}
			values[key] = value
		}
	}
	for labels, value := range values {
		ch <- prometheus.MustNewConstMetric(c.inflight, prometheus.GaugeValue, float64(value.count), labels[:]...)
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, value.oldest, labels[:]...)
		ch <- prometheus.MustNewConstMetric(c.overdue, prometheus.GaugeValue, float64(value.overdue), labels[:]...)
	}
}

func (c *setupInflightCollector) replaceProject(project string, entitlements []servicesv1alpha1.ServiceEntitlement) {
	entries := make([]setupInflightEntry, 0, len(entitlements))
	for i := range entitlements {
		entitlement, setup := &entitlements[i], entitlements[i].Status.Setup
		if setup == nil || setup.RequestedAt.IsZero() || setup.Outcome != "" {
			continue
		}
		entries = append(entries, setupInflightEntry{class: setupServiceClass(entitlement.Spec.ServiceRef.Name), operation: string(setup.Operation), blocker: setup.BlockingReason, requestedAt: setup.RequestedAt.Time})
	}
	c.mu.Lock()
	c.projects[project] = entries
	c.mu.Unlock()
}

func setupServiceClass(service string) string {
	switch {
	case strings.Contains(service, "compute"):
		return "compute"
	case strings.Contains(service, "network"):
		return "networking"
	case strings.Contains(service, "dns"):
		return "dns"
	case strings.Contains(service, "storage"):
		return "storage"
	default:
		return "other"
	}
}

func observeSetupSuccess(entitlement *servicesv1alpha1.ServiceEntitlement, completedAt time.Time) {
	setup := entitlement.Status.Setup
	if setup == nil || setup.RequestedAt.IsZero() {
		return
	}
	class, operation := setupServiceClass(entitlement.Spec.ServiceRef.Name), string(setup.Operation)
	duration := completedAt.Sub(setup.RequestedAt.Time).Seconds()
	if duration < 0 {
		return
	}
	setupDuration.WithLabelValues(class, operation, "user").Observe(duration)
	if setup.AuthorizedAt != nil {
		if d := completedAt.Sub(setup.AuthorizedAt.Time).Seconds(); d >= 0 {
			setupDuration.WithLabelValues(class, operation, "platform").Observe(d)
		}
	}
	setupAttempts.WithLabelValues(class, operation, string(servicesv1alpha1.ServiceSetupOutcomeSucceeded)).Inc()
}

func setupMetricSeriesBudget() int {
	const serviceClasses, operations, clocks, outcomes = 5, 3, 2, 4
	const setupBuckets, projectionPhases, projectionBuckets, blockingStages = 14, 7, 14, 5
	return serviceClasses*operations*clocks*setupBuckets + serviceClasses*operations*outcomes + projectionPhases*projectionBuckets + serviceClasses*operations*blockingStages*3 + 2
}
