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

const (
	setupClassCompute     = "compute"
	setupClassNetworking  = "networking"
	setupClassDNS         = "dns"
	setupClassStorage     = "storage"
	setupClassOther       = "other"
	setupOperationInitial = "initial"
	setupClockUser        = "user"
	setupClockPlatform    = "platform"
	setupOutcomeSucceeded = "succeeded"

	// setupOverdueAfter is the initial setup objective, measured from
	// authorization.
	setupOverdueAfter = time.Minute
	// setupStalledAfter separates setup that is slow from setup that is stuck.
	// An attempt still unfinished an hour after authorization is waiting on
	// something retries will not fix, such as a service with no available
	// locations, so it is reported apart from work that is merely slow.
	setupStalledAfter = time.Hour
)

var (
	setupServiceClasses       = []string{setupClassCompute, setupClassNetworking, setupClassDNS, setupClassStorage, setupClassOther}
	setupOperations           = []string{setupOperationInitial}
	setupClocks               = []string{setupClockUser, setupClockPlatform}
	setupOutcomes             = []string{setupOutcomeSucceeded}
	setupDurationBuckets      = []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, 86400}
	setupDuration             = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "service_catalog_setup_duration_seconds", Help: "Elapsed time from the original accepted request to successful catalog setup.", Buckets: setupDurationBuckets}, []string{"service_class", "operation", "clock"})
	setupAttempts             = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "service_catalog_setup_attempts_total", Help: "Terminal service setup attempts observed by the aggregate writer."}, []string{"service_class", "operation", "outcome"})
	projectionDurationBuckets = prometheus.ExponentialBuckets(0.005, 2, 12)
	projectionPhaseDuration   = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "service_catalog_projection_phase_duration_seconds", Help: "Time spent in bounded projection reconciliation phases.", Buckets: projectionDurationBuckets}, []string{"phase"})
	setupObserverLastSuccess  = prometheus.NewGauge(prometheus.GaugeOpts{Name: "service_catalog_setup_observer_last_success_timestamp_seconds", Help: "Unix timestamp of the last successful setup aggregation pass."})
	setupObserverHealthy      = prometheus.NewGauge(prometheus.GaugeOpts{Name: "service_catalog_setup_observer_healthy", Help: "Whether required setup source caches are synchronized on this projection owner."})
	setupInflight             = newSetupInflightCollector()
)

func init() {
	initializeSetupMetricSeries()
	crmetrics.Registry.MustRegister(setupDuration, setupAttempts, projectionPhaseDuration, setupObserverLastSuccess, setupObserverHealthy, setupInflight)
}

// initializeSetupMetricSeries exposes a zero-valued baseline for every bounded
// setup label combination from the first scrape. Without it, increase() cannot
// calculate the first completion when the series begins at its first increment.
func initializeSetupMetricSeries() {
	for _, class := range setupServiceClasses {
		for _, operation := range setupOperations {
			for _, clock := range setupClocks {
				setupDuration.WithLabelValues(class, operation, clock)
			}
			for _, outcome := range setupOutcomes {
				setupAttempts.WithLabelValues(class, operation, outcome)
			}
		}
	}
}

type setupInflightEntry struct {
	class, operation, blocker string
	requestedAt               time.Time
	// authorizedAt starts the overdue clock. It is zero while access awaits
	// external approval, which is reported as in flight but never overdue.
	authorizedAt time.Time
}

type setupInflightCollector struct {
	mu                                 sync.RWMutex
	projects                           map[string][]setupInflightEntry
	inflight, oldest, overdue, stalled *prometheus.Desc
}

func newSetupInflightCollector() *setupInflightCollector {
	labels := []string{"service_class", "operation", "blocking_stage"}
	return &setupInflightCollector{
		projects: make(map[string][]setupInflightEntry),
		inflight: prometheus.NewDesc("service_catalog_setup_inflight", "Unfinished persisted setup attempts.", labels, nil),
		oldest:   prometheus.NewDesc("service_catalog_setup_oldest_inflight_seconds", "Age of the oldest unfinished persisted setup attempt.", labels, nil),
		overdue:  prometheus.NewDesc("service_catalog_setup_overdue", "Unfinished setup attempts more than 60 seconds past authorization, the initial objective.", labels, nil),
		stalled:  prometheus.NewDesc("service_catalog_setup_stalled", "Unfinished setup attempts more than one hour past authorization, which are stuck rather than slow.", labels, nil),
	}
}

func (c *setupInflightCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.inflight
	ch <- c.oldest
	ch <- c.overdue
	ch <- c.stalled
}

func (c *setupInflightCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	type aggregate struct {
		count, overdue, stalled int
		oldest                  float64
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
			if !entry.authorizedAt.IsZero() {
				sinceAuthorized := now.Sub(entry.authorizedAt)
				if sinceAuthorized > setupOverdueAfter {
					value.overdue++
				}
				if sinceAuthorized > setupStalledAfter {
					value.stalled++
				}
			}
			values[key] = value
		}
	}
	for labels, value := range values {
		ch <- prometheus.MustNewConstMetric(c.inflight, prometheus.GaugeValue, float64(value.count), labels[:]...)
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, value.oldest, labels[:]...)
		ch <- prometheus.MustNewConstMetric(c.overdue, prometheus.GaugeValue, float64(value.overdue), labels[:]...)
		ch <- prometheus.MustNewConstMetric(c.stalled, prometheus.GaugeValue, float64(value.stalled), labels[:]...)
	}
}

func (c *setupInflightCollector) replaceProject(project string, entitlements []servicesv1alpha1.ServiceEntitlement) {
	entries := make([]setupInflightEntry, 0, len(entitlements))
	for i := range entitlements {
		entitlement, setup := &entitlements[i], entitlements[i].Status.Setup
		if setup == nil || setup.RequestedAt.IsZero() || setup.FirstCompletedAt != nil || setup.Backfilled {
			continue
		}
		// A rejected or deleting entitlement will never complete setup, so
		// counting it would leave permanent phantom work.
		if !entitlement.DeletionTimestamp.IsZero() || entitlement.Status.Phase == servicesv1alpha1.EntitlementPhaseRejected {
			continue
		}
		entry := setupInflightEntry{class: setupServiceClass(entitlement.Spec.ServiceRef.Name), operation: setupOperationInitial, blocker: firstIncompleteSetupStage(setup.Stages), requestedAt: setup.RequestedAt.Time}
		if setup.AuthorizedAt != nil {
			entry.authorizedAt = setup.AuthorizedAt.Time
		}
		entries = append(entries, entry)
	}
	c.mu.Lock()
	c.projects[project] = entries
	c.mu.Unlock()
}

func (c *setupInflightCollector) removeProject(project string) {
	c.mu.Lock()
	delete(c.projects, project)
	c.mu.Unlock()
}

func setupServiceClass(service string) string {
	switch {
	case strings.Contains(service, "compute"):
		return setupClassCompute
	case strings.Contains(service, "network"):
		return setupClassNetworking
	case strings.Contains(service, "dns"):
		return setupClassDNS
	case strings.Contains(service, "storage"):
		return setupClassStorage
	default:
		return setupClassOther
	}
}

func observeSetupSuccess(entitlement *servicesv1alpha1.ServiceEntitlement, completedAt time.Time) {
	setup := entitlement.Status.Setup
	if setup == nil || setup.RequestedAt.IsZero() || setup.Backfilled {
		return
	}
	class, operation := setupServiceClass(entitlement.Spec.ServiceRef.Name), setupOperationInitial
	duration := completedAt.Sub(setup.RequestedAt.Time).Seconds()
	if duration < 0 {
		return
	}
	setupDuration.WithLabelValues(class, operation, setupClockUser).Observe(duration)
	if setup.AuthorizedAt != nil {
		if d := completedAt.Sub(setup.AuthorizedAt.Time).Seconds(); d >= 0 {
			setupDuration.WithLabelValues(class, operation, setupClockPlatform).Observe(d)
		}
	}
	setupAttempts.WithLabelValues(class, operation, setupOutcomeSucceeded).Inc()
}

func setupMetricSeriesBudget() int {
	const projectionPhases, blockingStages = 7, 5
	serviceClasses, operations := len(setupServiceClasses), len(setupOperations)
	clocks, outcomes := len(setupClocks), len(setupOutcomes)
	setupBuckets := len(setupDurationBuckets) + 3 // explicit buckets plus +Inf, sum, and count
	projectionBuckets := len(projectionDurationBuckets) + 3
	return serviceClasses*operations*clocks*setupBuckets + serviceClasses*operations*outcomes + projectionPhases*projectionBuckets + serviceClasses*operations*blockingStages*4 + 2
}
