// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Per-service product metrics. The setup metrics in setup_metrics.go group
// services into a few broad classes for operators; these label by the actual
// service so product dashboards can answer which services projects use and how
// well each one activates.
//
// The service label is bounded by the catalog: a name that does not resolve
// to a Service is reported as serviceLabelUnknown, so a typo in an entitlement
// cannot mint a new series. Projects are never a label.
const (
	serviceLabelUnknown        = "unknown"
	serviceActivationSucceeded = "succeeded"
	serviceActivationRejected  = "rejected"
	servicePhasePending        = "Pending"
	serviceOriginUnknown       = "Unknown"
)

var (
	serviceActivationOutcomes = []string{serviceActivationSucceeded, serviceActivationRejected}
	serviceOrigins            = []string{string(servicesv1alpha1.EntitlementOriginDirect), string(servicesv1alpha1.EntitlementOriginDependency), serviceOriginUnknown}
	// The low buckets resolve the sub-second activation objective; the high
	// ones keep the long tail visible without the setup histogram's density.
	serviceSetupDurationBuckets = []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800, 3600, 21600, 86400}
	serviceSetupDuration        = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "service_catalog_service_setup_duration_seconds", Help: "Elapsed time from the original accepted request to successful setup, by service.", Buckets: serviceSetupDurationBuckets}, []string{"service", "clock"})
	serviceActivations          = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "service_catalog_service_activations_total", Help: "Terminal activation outcomes by service: first successful setup, or rejection."}, []string{"service", "outcome"})
	serviceDisables             = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "service_catalog_service_disables_total", Help: "ServiceEntitlements finalized after deletion, by service and origin."}, []string{"service", "origin"})
	serviceUsage                = newServiceUsageCollector()
)

func init() {
	crmetrics.Registry.MustRegister(serviceSetupDuration, serviceActivations, serviceDisables, serviceUsage)
}

// serviceMetricName returns the bounded service label for an entitlement.
func serviceMetricName(services map[string]*servicesv1alpha1.Service, entitlement *servicesv1alpha1.ServiceEntitlement) string {
	service := services[entitlement.Spec.ServiceRef.Name]
	if service == nil && entitlement.Status.ServiceName != "" {
		service = services[entitlement.Status.ServiceName]
	}
	return serviceLabel(service)
}

func serviceLabel(service *servicesv1alpha1.Service) string {
	switch {
	case service == nil, service.Spec.ServiceName == "" && service.Name == "":
		return serviceLabelUnknown
	case service.Spec.ServiceName != "":
		return service.Spec.ServiceName
	default:
		return service.Name
	}
}

func serviceOriginLabel(origin servicesv1alpha1.EntitlementOrigin) string {
	switch origin {
	case servicesv1alpha1.EntitlementOriginDirect, servicesv1alpha1.EntitlementOriginDependency:
		return string(origin)
	default:
		return serviceOriginUnknown
	}
}

// initializeServiceMetricSeries exposes zero-valued counters for every catalog
// service, so increase() sees the first activation or disable of a service
// instead of starting the series at it.
func initializeServiceMetricSeries(services map[string]*servicesv1alpha1.Service) {
	for _, service := range services {
		name := serviceLabel(service)
		for _, outcome := range serviceActivationOutcomes {
			serviceActivations.WithLabelValues(name, outcome)
		}
		for _, origin := range serviceOrigins {
			serviceDisables.WithLabelValues(name, origin)
		}
	}
}

func observeServiceSetupSuccess(service string, entitlement *servicesv1alpha1.ServiceEntitlement, completedAt time.Time) {
	setup := entitlement.Status.Setup
	if setup == nil || setup.RequestedAt.IsZero() || setup.Backfilled {
		return
	}
	duration := completedAt.Sub(setup.RequestedAt.Time).Seconds()
	if duration < 0 {
		return
	}
	serviceSetupDuration.WithLabelValues(service, setupClockUser).Observe(duration)
	if setup.AuthorizedAt != nil {
		if d := completedAt.Sub(setup.AuthorizedAt.Time).Seconds(); d >= 0 {
			serviceSetupDuration.WithLabelValues(service, setupClockPlatform).Observe(d)
		}
	}
	serviceActivations.WithLabelValues(service, serviceActivationSucceeded).Inc()
}

// observeServiceRejection counts an entitlement's move into Rejected. The
// canonical service name is only recorded once the Service resolves, so a
// request for a missing service counts under serviceLabelUnknown.
func observeServiceRejection(before, after *servicesv1alpha1.ServiceEntitlement) {
	if before.Status.Phase == servicesv1alpha1.EntitlementPhaseRejected || after.Status.Phase != servicesv1alpha1.EntitlementPhaseRejected {
		return
	}
	service := after.Status.ServiceName
	if service == "" {
		service = serviceLabelUnknown
	}
	serviceActivations.WithLabelValues(service, serviceActivationRejected).Inc()
}

func observeServiceDisable(service *servicesv1alpha1.Service, entitlement *servicesv1alpha1.ServiceEntitlement) {
	name := serviceLabel(service)
	if name == serviceLabelUnknown && entitlement.Status.ServiceName != "" {
		name = entitlement.Status.ServiceName
	}
	serviceDisables.WithLabelValues(name, serviceOriginLabel(entitlement.Status.Origin)).Inc()
}

type serviceUsageEntry struct {
	service, phase, origin string
	// blocker is the first incomplete setup stage, or empty once setup has
	// completed or when the attempt cannot finish.
	blocker      string
	authorizedAt time.Time
}

// serviceUsageCollector reports the current state of every entitlement by
// service, from the same per-project snapshot the setup aggregator already
// builds. Each projection owner reports only the projects it owns, so sum
// across instances.
type serviceUsageCollector struct {
	mu                              sync.RWMutex
	projects                        map[string][]serviceUsageEntry
	entitlements, inflight, stalled *prometheus.Desc
}

func newServiceUsageCollector() *serviceUsageCollector {
	return &serviceUsageCollector{
		projects:     make(map[string][]serviceUsageEntry),
		entitlements: prometheus.NewDesc("service_catalog_service_entitlements", "ServiceEntitlements that are not being deleted, by service, phase, and origin.", []string{"service", "phase", "origin"}, nil),
		inflight:     prometheus.NewDesc("service_catalog_service_setup_inflight", "Unfinished setup attempts by service and first incomplete stage.", []string{"service", "blocking_stage"}, nil),
		stalled:      prometheus.NewDesc("service_catalog_service_setup_stalled", "Unfinished setup attempts more than one hour past authorization, by service.", []string{"service"}, nil),
	}
}

func (c *serviceUsageCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.entitlements
	ch <- c.inflight
	ch <- c.stalled
}

func (c *serviceUsageCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entitlements := make(map[[3]string]int)
	inflight := make(map[[2]string]int)
	stalled := make(map[string]int)
	now := time.Now()
	for _, entries := range c.projects {
		for _, entry := range entries {
			entitlements[[3]string{entry.service, entry.phase, entry.origin}]++
			if entry.blocker == "" {
				continue
			}
			inflight[[2]string{entry.service, entry.blocker}]++
			if !entry.authorizedAt.IsZero() && now.Sub(entry.authorizedAt) > setupStalledAfter {
				stalled[entry.service]++
			}
		}
	}
	for labels, count := range entitlements {
		ch <- prometheus.MustNewConstMetric(c.entitlements, prometheus.GaugeValue, float64(count), labels[:]...)
	}
	for labels, count := range inflight {
		ch <- prometheus.MustNewConstMetric(c.inflight, prometheus.GaugeValue, float64(count), labels[:]...)
	}
	for service, count := range stalled {
		ch <- prometheus.MustNewConstMetric(c.stalled, prometheus.GaugeValue, float64(count), service)
	}
}

func (c *serviceUsageCollector) replaceProject(project string, entitlements []servicesv1alpha1.ServiceEntitlement, services map[string]*servicesv1alpha1.Service) {
	entries := make([]serviceUsageEntry, 0, len(entitlements))
	for i := range entitlements {
		entitlement := &entitlements[i]
		// A deleting entitlement is being disabled; it no longer counts as use.
		if !entitlement.DeletionTimestamp.IsZero() {
			continue
		}
		phase := string(entitlement.Status.Phase)
		if phase == "" {
			phase = servicePhasePending
		}
		entry := serviceUsageEntry{
			service: serviceMetricName(services, entitlement),
			phase:   phase,
			origin:  serviceOriginLabel(entitlement.Status.Origin),
		}
		// Same exclusions as the setup in-flight collector: finished,
		// backfilled, and rejected attempts are not work in progress.
		if setup := entitlement.Status.Setup; setup != nil && !setup.RequestedAt.IsZero() && setup.FirstCompletedAt == nil && !setup.Backfilled &&
			entitlement.Status.Phase != servicesv1alpha1.EntitlementPhaseRejected {
			entry.blocker = firstIncompleteSetupStage(setup.Stages)
			if setup.AuthorizedAt != nil {
				entry.authorizedAt = setup.AuthorizedAt.Time
			}
		}
		entries = append(entries, entry)
	}
	c.mu.Lock()
	c.projects[project] = entries
	c.mu.Unlock()
}

func (c *serviceUsageCollector) removeProject(project string) {
	c.mu.Lock()
	delete(c.projects, project)
	c.mu.Unlock()
}
