// Service Catalog Setup Dashboard
//
// Operational view of customer-visible setup latency, unfinished work, the
// elected projection path, and the Kubernetes client pressure that can delay
// convergence. Empty-state fallbacks are deliberate: a healthy observer with
// no work reports zero, while missing or unhealthy setup telemetry reports -1
// and is rendered as "Telemetry unavailable" rather than a healthy zero.

local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet';

local prometheus = g.query.prometheus;
local stat = g.panel.stat;
local timeSeries = g.panel.timeSeries;
local row = g.panel.row;
local var = g.dashboard.variable;

local ds = '${datasource}';
local job = 'job="services-system/services-controller-manager"';
local controllerPods = 'namespace="services-system",container="manager",pod=~"services-controller-manager-.*"';
local projectionQueue = job + ',name=~"location-(binding|projection)"';
local projectionController = job + ',controller=~"location-(binding|projection)"';
local unfinishedByStage = '(sum by (blocking_stage) (service_catalog_setup_inflight)) and on() (max(service_catalog_setup_observer_healthy) == 1)';

local prom(expr, legend='') =
  prometheus.new(ds, expr)
  + prometheus.withLegendFormat(legend);

local pos(h, w, x, y) = { gridPos: { h: h, w: w, x: x, y: y } };
local steps(values) = [{ color: value[0], value: value[1] } for value in values];

local telemetryMappings(noObservation='No observations') = [
  { type: 'value', options: { '-1': { text: 'Telemetry unavailable', color: 'purple' } } },
  { type: 'special', options: { match: 'null', result: { text: noObservation, color: 'text' } } },
  { type: 'special', options: { match: 'nan', result: { text: noObservation, color: 'text' } } },
];

local observerMappings = [
  { type: 'value', options: {
    '-1': { text: 'Not reported', color: 'purple' },
    '0': { text: 'Unhealthy', color: 'red' },
    '1': { text: 'Healthy', color: 'green' },
  } },
];

// A setup aggregate is zero only when the observer is healthy. A missing or
// unhealthy observer produces -1 so an empty or stale cache cannot look healthy.
local observedAggregate(expr) =
  '((%s) and on() (max(service_catalog_setup_observer_healthy) == 1)) or ((max(service_catalog_setup_observer_healthy) == 1) * 0) or ((max(service_catalog_setup_observer_healthy) == 0) - 1) or (absent(service_catalog_setup_observer_healthy) * -1)' % expr;

local completedAggregate(expr) = observedAggregate(expr);

local statPanel(title, expr, legend, unit, thresholds, gridPos,
                description, mappings=[], graphMode='none') =
  stat.new(title)
  + pos(gridPos.h, gridPos.w, gridPos.x, gridPos.y)
  + stat.panelOptions.withDescription(description)
  + stat.queryOptions.withDatasource('prometheus', ds)
  + stat.queryOptions.withTargets([prom(expr, legend)])
  + stat.options.withColorMode('background')
  + stat.options.withGraphMode(graphMode)
  + stat.options.withTextMode('auto')
  + stat.options.reduceOptions.withCalcs(['lastNotNull'])
  + stat.standardOptions.withUnit(unit)
  + stat.standardOptions.withNoValue('No observations')
  + stat.standardOptions.withMappings(mappings)
  + stat.standardOptions.color.withMode('thresholds')
  + stat.standardOptions.thresholds.withMode('absolute')
  + stat.standardOptions.thresholds.withSteps(steps(thresholds));

local timePanel(title, targets, unit, gridPos, description, stacking='none', legendCalcs=['lastNotNull', 'max']) =
  timeSeries.new(title)
  + pos(gridPos.h, gridPos.w, gridPos.x, gridPos.y)
  + timeSeries.panelOptions.withDescription(description)
  + timeSeries.queryOptions.withDatasource('prometheus', ds)
  + timeSeries.queryOptions.withTargets(targets)
  + timeSeries.standardOptions.withUnit(unit)
  + timeSeries.standardOptions.withNoValue('No observations in range')
  + timeSeries.fieldConfig.defaults.custom.withDrawStyle('line')
  + timeSeries.fieldConfig.defaults.custom.withLineInterpolation('smooth')
  + timeSeries.fieldConfig.defaults.custom.withLineWidth(2)
  + timeSeries.fieldConfig.defaults.custom.withFillOpacity(if stacking == 'normal' then 25 else 10)
  + timeSeries.fieldConfig.defaults.custom.withShowPoints('never')
  + timeSeries.fieldConfig.defaults.custom.stacking.withMode(stacking)
  + timeSeries.fieldConfig.defaults.custom.stacking.withGroup('A')
  + timeSeries.options.tooltip.withMode('multi')
  + timeSeries.options.tooltip.withSort('desc')
  + timeSeries.options.legend.withDisplayMode('table')
  + timeSeries.options.legend.withPlacement('right')
  + timeSeries.options.legend.withCalcs(legendCalcs)
  + timeSeries.options.legend.withSortBy('Max')
  + timeSeries.options.legend.withSortDesc(true);

local section(title, id, y) =
  row.new(title)
  + row.withId(id)
  + pos(1, 24, 0, y)
  + row.withCollapsed(false);

local collapsedSection(title, id, y, panels) =
  row.new(title)
  + row.withId(id)
  + pos(1, 24, 0, y)
  + row.withCollapsed(true)
  + row.withPanels(panels);

local datasourceVar =
  var.datasource.new('datasource', 'prometheus')
  + var.datasource.generalOptions.withLabel('Data Source')
  + var.datasource.generalOptions.withCurrent('Service Metrics', 'SERVICE_METRICS')
  + var.datasource.generalOptions.showOnDashboard.withLabelAndValue()
  + var.datasource.withRegex('');

local panels = [
  section('Customer outcomes', 100, 0),
  statPanel(
    'User setup p95',
    'histogram_quantile(0.95, sum by (le) (increase(service_catalog_setup_duration_seconds_bucket{clock="user"}[$__range]))) or vector(-1)',
    'user p95', 's', [['purple', null], ['green', 0], ['yellow', 60], ['red', 300]],
    { h: 5, w: 6, x: 0, y: 1 },
    'Request acceptance to first successful SetupComplete for completions in the selected range. -1 means the telemetry is not deployed or no successful setup exists in the range.',
    telemetryMappings('No completed setups'), 'area'),
  statPanel(
    'Platform setup p95',
    'histogram_quantile(0.95, sum by (le) (increase(service_catalog_setup_duration_seconds_bucket{clock="platform"}[$__range]))) or vector(-1)',
    'platform p95', 's', [['purple', null], ['green', 0], ['yellow', 60], ['red', 300]],
    { h: 5, w: 6, x: 6, y: 1 },
    'Authorization to first successful SetupComplete for completions in the selected range. External approval time is excluded. -1 means no observation is available.',
    telemetryMappings('No completed setups'), 'area'),
  statPanel(
    'Completed setups',
    completedAggregate('sum(increase(service_catalog_setup_attempts_total{outcome="succeeded"}[$__range]))'),
    'completed', 'short', [['purple', null], ['green', 0]],
    { h: 5, w: 6, x: 12, y: 1 },
    'Successful initial setup attempts in the selected dashboard range. Zero is valid only while observer telemetry is present.', telemetryMappings()),
  statPanel(
    'Replicas scraped',
    '(count(up{%s} == 1) / clamp_min(count(up{%s}), 1)) or vector(0)' % [job, job],
    'scrape coverage', 'percentunit', [['red', null], ['yellow', 0.5], ['green', 1]],
    { h: 5, w: 6, x: 18, y: 1 },
    'Fraction of discovered service-catalog controller targets that VictoriaMetrics is scraping successfully.', [], 'area'),
  timePanel(
    'Setup latency by service class and clock',
    [prom('(histogram_quantile(0.95, sum by (le, service_class, clock) (rate(service_catalog_setup_duration_seconds_bucket[$__rate_interval])))) or (label_replace(label_replace(absent(histogram_quantile(0.95, sum by (le, service_class, clock) (rate(service_catalog_setup_duration_seconds_bucket[$__rate_interval])))) * -1, "service_class", "none", "", ""), "clock", "no observations", "", ""))', 'p95 {{service_class}} / {{clock}}')],
    's', { h: 8, w: 24, x: 0, y: 6 },
    'End-to-end p95. The -1 fallback keeps the panel explicit before telemetry rollout or during an idle range; use Observer health before interpreting it.'),

  section('Unfinished work', 200, 14),
  statPanel('In flight', observedAggregate('sum(service_catalog_setup_inflight)'), 'in flight', 'short',
    [['purple', null], ['green', 0], ['yellow', 1], ['red', 10]], { h: 5, w: 8, x: 0, y: 15 },
    'Persisted setup attempts that have not reached first completion. Missing or unhealthy observer telemetry renders Telemetry unavailable.', telemetryMappings()),
  statPanel('Overdue (>60s)', observedAggregate('sum(service_catalog_setup_overdue)'), 'overdue', 'short',
    [['purple', null], ['green', 0], ['yellow', 1], ['red', 5]], { h: 5, w: 8, x: 8, y: 15 },
    'Unfinished attempts older than the initial 60-second objective.', telemetryMappings()),
  statPanel('Oldest unfinished', observedAggregate('max(service_catalog_setup_oldest_inflight_seconds)'), 'oldest', 's',
    [['purple', null], ['green', 0], ['yellow', 60], ['red', 300]], { h: 5, w: 8, x: 16, y: 15 },
    'Age of the oldest unfinished persisted setup attempt.', telemetryMappings()),
  timePanel(
    'Unfinished attempts by blocking stage over time',
    [prom('(%s) or label_replace((absent(%s) * (max(service_catalog_setup_observer_healthy) == 1)) * 0, "blocking_stage", "none", "", "") or label_replace(absent(%s) * (((max(service_catalog_setup_observer_healthy) == 0) - 1) or (absent(service_catalog_setup_observer_healthy) * -1)), "blocking_stage", "telemetry unavailable", "", "")' % [unfinishedByStage, unfinishedByStage, unfinishedByStage], '{{blocking_stage}}')],
    'short', { h: 8, w: 24, x: 0, y: 20 },
    'Unfinished attempts grouped by the first incomplete stage over the selected range. The legend reports only the range maximum because a disappeared stage has no trustworthy current sample. -1 means coverage is unavailable; a healthy empty observer reports zero.', 'normal', ['max']),

  section('Stage breakdown', 300, 28),
  timePanel(
    'Projection phase p95',
    [prom('(histogram_quantile(0.95, sum by (le, phase) (rate(service_catalog_projection_phase_duration_seconds_bucket[$__rate_interval])))) or label_replace(absent(histogram_quantile(0.95, sum by (le, phase) (rate(service_catalog_projection_phase_duration_seconds_bucket[$__rate_interval])))) * -1, "phase", "no observations", "", "")', '{{phase}}')],
    's', { h: 8, w: 12, x: 0, y: 29 },
    'Execution time for bounded projection phases. This does not include workqueue delay.'),
  timePanel(
    'Setup completion volume by class',
    [prom('(sum by (service_class) (increase(service_catalog_setup_attempts_total{outcome="succeeded"}[$__interval]))) or label_replace(absent(sum by (service_class) (increase(service_catalog_setup_attempts_total{outcome="succeeded"}[$__interval]))) * 0, "service_class", "none", "", "")', '{{service_class}}')],
    'short', { h: 8, w: 12, x: 12, y: 29 },
    'Successful setup attempts per interval and service class. Each completion is counted once. Zero means no completion was recorded; observer health shows whether coverage is trustworthy.', 'normal'),

  section('Controller capacity and queueing', 400, 37),
  statPanel('Projection queue depth', 'sum(workqueue_depth{%s}) or vector(0)' % projectionQueue, 'queued', 'short',
    [['green', null], ['yellow', 10], ['red', 50]], { h: 5, w: 6, x: 0, y: 38 },
    'Projects waiting in the projection queue. The selector spans the legacy location-binding and new location-projection controller names.', [], 'area'),
  statPanel('Queue delay p95', 'histogram_quantile(0.95, sum by (le) (rate(workqueue_queue_duration_seconds_bucket{%s}[$__rate_interval]))) or vector(0)' % projectionQueue, 'p95', 's',
    [['green', null], ['yellow', 1], ['red', 5]], { h: 5, w: 6, x: 6, y: 38 },
    'Time from enqueue to worker start. Zero means no queue-delay observations in the rate window.', [], 'area'),
  statPanel('Reconcile execution p95', 'histogram_quantile(0.95, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{%s}[$__rate_interval]))) or vector(0)' % projectionController, 'p95', 's',
    [['green', null], ['yellow', 1], ['red', 5]], { h: 5, w: 6, x: 12, y: 38 },
    'Worker execution time after dequeue. Compare this with queue delay to distinguish capacity from slow reconciliation.', [], 'area'),
  statPanel('Active projection workers', 'sum(controller_runtime_active_workers{%s}) or vector(0)' % projectionController, 'active', 'short',
    [['green', null], ['yellow', 4], ['red', 8]], { h: 5, w: 6, x: 18, y: 38 },
    'Active workers across the selected projection controller. Under elected projection, only the owner should process work.', [], 'area'),
  timePanel(
    'Queue delay and worker execution',
    [
      prom('histogram_quantile(0.95, sum by (le) (rate(workqueue_queue_duration_seconds_bucket{%s}[$__rate_interval]))) or vector(0)' % projectionQueue, 'queue p95'),
      prom('histogram_quantile(0.95, sum by (le) (rate(workqueue_work_duration_seconds_bucket{%s}[$__rate_interval]))) or vector(0)' % projectionQueue, 'work p95'),
      prom('max(workqueue_longest_running_processor_seconds{%s}) or vector(0)' % projectionQueue, 'longest active'),
    ],
    's', { h: 8, w: 16, x: 0, y: 43 },
    'Queueing, execution, and the longest active worker. A growing queue delay with bounded work time indicates insufficient effective ownership or worker capacity.'),
  timePanel(
    'Projection queue depth and retries',
    [
      prom('sum(workqueue_depth{%s}) or vector(0)' % projectionQueue, 'depth'),
      prom('sum(rate(workqueue_retries_total{%s}[$__rate_interval])) or vector(0)' % projectionQueue, 'retries/s'),
    ],
    'short', { h: 8, w: 8, x: 16, y: 43 },
    'Outstanding projects and retry pressure. Both series use bounded controller names and carry no project labels.'),

  section('Observer health', 500, 51),
  statPanel('Setup observer', 'max(service_catalog_setup_observer_healthy) or vector(-1)', 'observer', 'short',
    [['purple', null], ['red', 0], ['green', 1]], { h: 5, w: 8, x: 0, y: 52 },
    'Whether all source caches needed to interpret setup state are synchronized on the projection owner. Missing data is explicitly unknown.', observerMappings),
  statPanel('Last successful observation', '(time() - max(service_catalog_setup_observer_last_success_timestamp_seconds)) or vector(-1)', 'age', 's',
    [['purple', null], ['green', 0], ['yellow', 120], ['red', 300]], { h: 5, w: 8, x: 8, y: 52 },
    'Seconds since the aggregate writer last completed a successful observation pass. -1 means it has never reported.', telemetryMappings('Never reported'), 'area'),
  statPanel('Controller targets up', 'count(up{%s} == 1) or vector(0)' % job, 'up', 'short',
    [['red', null], ['yellow', 1], ['green', 2]], { h: 5, w: 8, x: 16, y: 52 },
    'Number of service-catalog controller targets currently scraped as up. This proves scrape reachability, not cache completeness.', [], 'area'),

  collapsedSection('Leadership', 600, 57, [
  statPanel('Projection leaders', 'sum(leader_election_master_status{%s,name=~"(consumer|project)-projection[.]services[.]miloapis[.]com"}) or vector(-1)' % job, 'leaders', 'short',
    [['purple', null], ['red', 0], ['green', 1], ['red', 2]], { h: 5, w: 8, x: 0, y: 58 },
    'Exactly one replica should own the projection lease after rollout. -1 means this release has not exposed the distinct projection election.', telemetryMappings('Not reported')),
  statPanel('Primary manager leaders', 'sum(leader_election_master_status{%s,name="services.miloapis.com"}) or vector(-1)' % job, 'leaders', 'short',
    [['purple', null], ['red', 0], ['green', 1], ['red', 2]], { h: 5, w: 8, x: 8, y: 58 },
    'Exactly one primary controller manager leader is expected.', telemetryMappings('Not reported')),
  statPanel('Leader-bearing replicas', 'count(sum by (pod) (leader_election_master_status{%s} == 1)) or vector(0)' % job, 'replicas', 'short',
    [['red', null], ['green', 1], ['yellow', 2], ['red', 3]], { h: 5, w: 8, x: 16, y: 58 },
    'Replicas holding any service-catalog manager election. Separate primary and projection managers may intentionally land on different pods.', [], 'area'),
  timePanel(
    'Election ownership by manager',
    [prom('(sum by (name) (leader_election_master_status{%s})) or label_replace(absent(sum by (name) (leader_election_master_status{%s})) * -1, "name", "not reported", "", "")' % [job, job], '{{name}}')],
    'short', { h: 7, w: 24, x: 0, y: 63 },
    'Sum of leader-election status across replicas for each manager name. Each active election should remain at exactly one.'),
  ]),

  collapsedSection('Controller-wide API and throttling', 700, 58, [
  statPanel('Client throttle wait p95', '(histogram_quantile(0.95, sum by (le) (rate(rest_client_rate_limiter_duration_seconds_bucket{%s}[$__rate_interval])))) or (sum(absent(rest_client_rate_limiter_duration_seconds_bucket{%s})) * -1)' % [job, job], 'p95', 's',
    [['purple', null], ['green', 0], ['yellow', 0.1], ['red', 1]], { h: 5, w: 8, x: 0, y: 59 },
    'Controller-wide time spent waiting for client-side rate-limit tokens. Telemetry unavailable means the deployed client does not expose this histogram; it must not be interpreted as zero wait.', telemetryMappings('No throttle observations'), 'area'),
  statPanel('HTTP 429 rate', 'sum(rate(rest_client_requests_total{%s,code="429"}[$__rate_interval])) or vector(0)' % job, '429/s', 'reqps',
    [['green', null], ['yellow', 0.01], ['red', 0.1]], { h: 5, w: 8, x: 8, y: 59 },
    'Controller-wide Kubernetes API responses rejected for server-side throttling. Client metrics cannot attribute traffic to a specific reconciler.', [], 'area'),
  statPanel('API request p95', 'histogram_quantile(0.95, sum by (le) (rate(rest_client_request_duration_seconds_bucket{%s}[$__rate_interval]))) or vector(-1)' % job, 'p95', 's',
    [['purple', null], ['green', 0], ['yellow', 0.5], ['red', 2]], { h: 5, w: 8, x: 16, y: 59 },
    'Controller-wide Kubernetes REST round-trip latency. This excludes client rate-limiter wait and cannot be attributed to a specific reconciler. -1 means the histogram is unavailable on the deployed release.', telemetryMappings('No request observations'), 'area'),
  timePanel(
    'Kubernetes API request rate by response code',
    [prom('(sum by (code) (rate(rest_client_requests_total{%s}[$__rate_interval]))) or label_replace(absent(sum by (code) (rate(rest_client_requests_total{%s}[$__rate_interval]))) * 0, "code", "none", "", "")' % [job, job], '{{code}}')],
    'reqps', { h: 8, w: 12, x: 0, y: 64 },
    'Controller-wide REST traffic grouped by bounded HTTP response code. Correlation with setup delay is indirect because client metrics have no reconciler label.', 'normal'),
  timePanel(
    'Kubernetes API request latency by method',
    [prom('(histogram_quantile(0.95, sum by (le, verb) (rate(rest_client_request_duration_seconds_bucket{%s}[$__rate_interval])))) or label_replace(absent(histogram_quantile(0.95, sum by (le, verb) (rate(rest_client_request_duration_seconds_bucket{%s}[$__rate_interval])))) * -1, "verb", "telemetry unavailable", "", "")' % [job, job], '{{verb}}')],
    's', { h: 8, w: 12, x: 12, y: 64 },
    'Controller-wide p95 REST round-trip latency grouped by method label (`verb`). Client-side limiter waits are shown separately.'),
  ]),

  collapsedSection('Rollout and version', 800, 59, [
  statPanel('Controller restarts', 'sum(increase(kube_pod_container_status_restarts_total{%s}[$__range])) or vector(0)' % controllerPods, 'restarts', 'short',
    [['green', null], ['yellow', 1], ['red', 3]], { h: 5, w: 8, x: 0, y: 60 },
    'Container restarts during the selected range. Leadership loss intentionally terminates the process, so correlate restarts with election changes.', [], 'area'),
  statPanel('Ready controller pods', 'sum(kube_pod_status_ready{namespace="services-system",pod=~"services-controller-manager-.*",condition="true"}) or vector(0)', 'ready', 'short',
    [['red', null], ['yellow', 1], ['green', 2]], { h: 5, w: 8, x: 8, y: 60 },
    'Ready service-catalog controller-manager pods.'),
  statPanel('Process age (youngest)', 'min(time() - process_start_time_seconds{%s}) or vector(-1)' % job, 'age', 's',
    [['purple', null], ['red', 0], ['yellow', 300], ['green', 1800]], { h: 5, w: 8, x: 16, y: 60 },
    'Age of the youngest scraped controller process. A reset identifies rollout or restart timing.', telemetryMappings('Not reported'), 'area'),
  timePanel(
    'Replica count by image over selected range',
    [prom('(count by (image) (kube_pod_container_info{%s})) or label_replace(absent(count by (image) (kube_pod_container_info{%s})) * 0, "image", "not observed", "", "")' % [controllerPods, controllerPods], '{{image}}')],
    'short', { h: 8, w: 24, x: 0, y: 65 },
    'Replica count by immutable image reference over the selected range. Historical image series remain visible to show rollout transitions.'),
  ]),
];

g.dashboard.new('Service Catalog Setup')
+ g.dashboard.withUid('service-catalog-setup')
+ g.dashboard.withDescription('Customer-visible service setup latency and blockers, projection queue capacity, observer health, leader ownership, Kubernetes API pressure, and rollout state.')
+ g.dashboard.withTags(['milo', 'service-catalog', 'setup', 'platform'])
+ g.dashboard.withEditable(true)
+ g.dashboard.withRefresh('30s')
+ g.dashboard.withSchemaVersion(39)
+ g.dashboard.graphTooltip.withSharedCrosshair()
+ g.dashboard.time.withFrom('now-6h')
+ g.dashboard.time.withTo('now')
+ g.dashboard.withLinks([
  { icon: 'external link', includeVars: false, keepTime: false, targetBlank: true, title: 'Runbook', tooltip: 'Diagnose service setup stalls', type: 'link', url: 'https://github.com/datum-cloud/infra/blob/main/docs/runbooks/control-plane/service-setup.md' },
  { icon: 'dashboard', includeVars: true, keepTime: true, targetBlank: false, title: 'Milo API latency', tooltip: 'Control-plane API latency and dependencies', type: 'link', url: '/d/milo-api-latency' },
])
+ g.dashboard.withVariables([datasourceVar])
+ g.dashboard.withPanels(panels)
