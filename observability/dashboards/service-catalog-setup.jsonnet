// Service Catalog Setup Dashboard
//
// Answers "is setup fast and healthy right now, and if not, why?" The top row
// separates setup that is slow right now from setup stalled on a known
// blocker; the sections below show where setup time goes (controller queues,
// Kubernetes API latency versus client throttling) and the leader changes and
// restarts behind most outliers. Empty-state fallbacks are deliberate: a
// healthy observer with no work reports zero, while missing or unhealthy setup
// telemetry reports -1 and is rendered as "Telemetry unavailable" rather than
// a healthy zero.

local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet';

local prometheus = g.query.prometheus;
local stat = g.panel.stat;
local timeSeries = g.panel.timeSeries;
local row = g.panel.row;
local table = g.panel.table;
local var = g.dashboard.variable;

local ds = '${datasource}';
local job = 'job="services-system/services-controller-manager"';
local controllerPods = 'namespace="services-system",container="manager",pod=~"services-controller-manager-.*"';
// The controllers a setup passes through, in order.
local setupControllers = 'service-entitlement|service-provisioning|location-projection|capability-binding';
local setupQueues = job + ',name=~"%s"' % setupControllers;
local setupReconcilers = job + ',controller=~"%s"' % setupControllers;
// Project discovery: after a start or failover every project control plane is
// queued here, and setup in a project waits until its project is reconnected.
local projectReconnectQueue = job + ',name="projectcontrolplane"';
local observerHealthy = 'and on() (max(service_catalog_setup_observer_healthy) == 1)';
local unfinishedByStage = '(sum by (blocking_stage) (service_catalog_setup_inflight)) ' + observerHealthy;
// Setup that is overdue but not stalled: more than a minute and less than an
// hour past authorization. Releases before the stalled gauge count every
// overdue attempt here.
local slowByStage = '((sum by (service_class, blocking_stage) (service_catalog_setup_overdue) - (sum by (service_class, blocking_stage) (service_catalog_setup_stalled) or sum by (service_class, blocking_stage) (service_catalog_setup_overdue) * 0)) > 0) ' + observerHealthy;
// The stalled gauge is absent both when nothing is unfinished and on releases
// that predate it. Unfinished work without the gauge means the latter, which
// must read as unavailable rather than as zero stalled setups.
local stalledUnavailable = '(absent(service_catalog_setup_stalled) * -1 and on() count(service_catalog_setup_inflight))';
local clientMetricsPending = 'Not exported yet';
local clientMetricsIdleOrPending = 'No requests in range, or client-go %s metrics not exported yet';

local prom(expr, legend='') =
  prometheus.new(ds, expr)
  + prometheus.withLegendFormat(legend);

local pos(h, w, x, y) = { gridPos: { h: h, w: w, x: x, y: y } };
local steps(values) = [{ color: value[0], value: value[1] } for value in values];

local telemetryMappings(noObservation='No observations', unavailable='Telemetry unavailable') = [
  { type: 'value', options: { '-1': { text: unavailable, color: 'purple' } } },
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

local timePanel(title, targets, unit, gridPos, description, stacking='none', legendCalcs=['lastNotNull', 'max'], noValue='No observations in range') =
  timeSeries.new(title)
  + pos(gridPos.h, gridPos.w, gridPos.x, gridPos.y)
  + timeSeries.panelOptions.withDescription(description)
  + timeSeries.queryOptions.withDatasource('prometheus', ds)
  + timeSeries.queryOptions.withTargets(targets)
  + timeSeries.standardOptions.withUnit(unit)
  + timeSeries.standardOptions.withNoValue(noValue)
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

// Stalled setups as one row per service class and blocking stage, with the
// stage translated into what usually blocks it.
local stalledTable(gridPos) =
  table.new('Stalled setups by blocking stage')
  + pos(gridPos.h, gridPos.w, gridPos.x, gridPos.y)
  + table.panelOptions.withDescription('Setups still unfinished more than an hour after authorization. Retrying does not fix these: they wait on configuration such as a service with no available locations or provider resources that cannot be installed. Each row is a service class and the first incomplete setup stage, with the age of its oldest attempt. Empty also while the setup observer is unhealthy; check Setup observer first. Telemetry unavailable means setups are unfinished but the deployed release does not report which are stalled.')
  + table.queryOptions.withDatasource('prometheus', ds)
  + table.queryOptions.withTargets([
    prom('(sum by (service_class, blocking_stage) (service_catalog_setup_stalled > 0) ' + observerHealthy + ') or label_replace(' + stalledUnavailable + ', "blocking_stage", "Telemetry unavailable", "", "")')
    + prometheus.withInstant(true) + prometheus.withFormat('table') + prometheus.withRefId('stalled'),
    prom('max by (service_class, blocking_stage) (service_catalog_setup_oldest_inflight_seconds and on(service_class, operation, blocking_stage) (service_catalog_setup_stalled > 0)) ' + observerHealthy)
    + prometheus.withInstant(true) + prometheus.withFormat('table') + prometheus.withRefId('oldest'),
  ])
  + table.queryOptions.withTransformations([
    { id: 'merge', options: {} },
    { id: 'organize', options: {
      excludeByName: { Time: true },
      indexByName: { blocking_stage: 0, service_class: 1, 'Value #stalled': 2, 'Value #oldest': 3 },
      renameByName: { blocking_stage: 'Blocked at', service_class: 'Service class', 'Value #stalled': 'Stalled', 'Value #oldest': 'Oldest' },
    } },
  ])
  + table.standardOptions.withNoValue('No stalled setups')
  + table.standardOptions.withOverrides([
    { matcher: { id: 'byName', options: 'Oldest' }, properties: [{ id: 'unit', value: 'dtdurations' }] },
    { matcher: { id: 'byName', options: 'Stalled' }, properties: [{ id: 'mappings', value: [
      { type: 'value', options: { '-1': { text: 'Telemetry unavailable', color: 'purple' } } },
    ] }] },
    { matcher: { id: 'byName', options: 'Blocked at' }, properties: [{ id: 'mappings', value: [
      { type: 'value', options: {
        Authorization: { text: 'Authorization: access not approved, or project suspended' },
        Dependencies: { text: 'Dependencies: a required service has not finished setup' },
        Provisioning: { text: 'Provisioning: provider resources not installed (see the Provisioned condition)' },
        Projection: { text: 'Projection: no available locations, or availability not converged' },
        ProviderAck: { text: 'Provider acknowledgement pending' },
      } },
    ] }] },
  ])
  + table.options.withSortBy([{ displayName: 'Stalled', desc: true }]);

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
  section('Is setup fast and healthy?', 100, 0),
  statPanel(
    'User setup p95',
    'histogram_quantile(0.95, sum by (le) (increase(service_catalog_setup_duration_seconds_bucket{clock="user"}[$__range]))) or vector(-1)',
    'user p95', 's', [['purple', null], ['green', 0], ['yellow', 60], ['red', 300]],
    { h: 5, w: 4, x: 0, y: 1 },
    'Request acceptance to first successful SetupComplete for completions in the selected range. -1 means the telemetry is not deployed or no successful setup exists in the range.',
    telemetryMappings('No completed setups'), 'area'),
  statPanel(
    'Platform setup p95',
    'histogram_quantile(0.95, sum by (le) (increase(service_catalog_setup_duration_seconds_bucket{clock="platform"}[$__range]))) or vector(-1)',
    'platform p95', 's', [['purple', null], ['green', 0], ['yellow', 60], ['red', 300]],
    { h: 5, w: 4, x: 4, y: 1 },
    'Authorization to first successful SetupComplete for completions in the selected range. External approval time is excluded. -1 means no observation is available.',
    telemetryMappings('No completed setups'), 'area'),
  statPanel(
    'Completed setups',
    completedAggregate('sum(increase(service_catalog_setup_attempts_total{outcome="succeeded"}[$__range]))'),
    'completed', 'short', [['purple', null], ['green', 0]],
    { h: 5, w: 4, x: 8, y: 1 },
    'Successful initial setup attempts in the selected dashboard range. Zero is valid only while observer telemetry is present.', telemetryMappings()),
  statPanel('Slow right now',
    observedAggregate('sum(service_catalog_setup_overdue) - (sum(service_catalog_setup_stalled) or vector(0))'), 'slow', 'short',
    [['purple', null], ['green', 0], ['yellow', 1], ['red', 5]], { h: 5, w: 4, x: 12, y: 1 },
    'Unfinished setups between one minute and one hour past authorization: work that should finish but has not yet. This is the number to act on. Stalled setups are counted separately. Releases without the stalled gauge count every overdue setup here.', telemetryMappings()),
  statPanel('Stalled (>1h)', '%s or (%s)' % [stalledUnavailable, observedAggregate('sum(service_catalog_setup_stalled)')], 'stalled', 'short',
    [['purple', null], ['green', 0], ['orange', 1]], { h: 5, w: 4, x: 16, y: 1 },
    'Unfinished setups more than an hour past authorization. These wait on a known blocker, not on controller speed; see Stalled setups by blocking stage for what each one needs. Telemetry unavailable means setups are unfinished but the deployed release does not report which are stalled.', telemetryMappings()),
  statPanel('Setup observer', 'max(service_catalog_setup_observer_healthy) or vector(-1)', 'observer', 'short',
    [['purple', null], ['red', 0], ['green', 1]], { h: 5, w: 4, x: 20, y: 1 },
    'Whether all source caches needed to interpret setup state are synchronized on the projection owner. The unfinished-work numbers are trustworthy only while this is Healthy.', observerMappings),
  timePanel(
    'Setup latency by service class and clock',
    [prom('(histogram_quantile(0.95, sum by (le, service_class, clock) (rate(service_catalog_setup_duration_seconds_bucket[$__rate_interval])))) or (label_replace(label_replace(absent(histogram_quantile(0.95, sum by (le, service_class, clock) (rate(service_catalog_setup_duration_seconds_bucket[$__rate_interval])))) * -1, "service_class", "none", "", ""), "clock", "no observations", "", ""))', 'p95 {{service_class}} / {{clock}}')],
    's', { h: 7, w: 24, x: 0, y: 6 },
    'End-to-end p95. The -1 fallback keeps the panel explicit before telemetry rollout or during an idle range; use Setup observer before interpreting it. Annotations mark controller starts and leader acquisitions, which cause most outliers.'),

  section('Unfinished work', 200, 13),
  stalledTable({ h: 8, w: 12, x: 0, y: 14 }),
  timePanel(
    'Slow setups by stage',
    [prom('(%s) or label_replace((absent(%s) * (max(service_catalog_setup_observer_healthy) == 1)) * 0, "blocking_stage", "none", "", "")' % [slowByStage, slowByStage], '{{service_class}} at {{blocking_stage}}')],
    'short', { h: 8, w: 12, x: 12, y: 14 },
    'Setups between one minute and one hour past authorization, by service class and the stage they are waiting on. Zero means nothing is slow; stalled setups are excluded.', 'normal', ['max']),
  timePanel(
    'Unfinished attempts by blocking stage over time',
    [prom('(%s) or label_replace((absent(%s) * (max(service_catalog_setup_observer_healthy) == 1)) * 0, "blocking_stage", "none", "", "") or label_replace(absent(%s) * (((max(service_catalog_setup_observer_healthy) == 0) - 1) or (absent(service_catalog_setup_observer_healthy) * -1)), "blocking_stage", "telemetry unavailable", "", "")' % [unfinishedByStage, unfinishedByStage, unfinishedByStage], '{{blocking_stage}}')],
    'short', { h: 7, w: 24, x: 0, y: 22 },
    'All unfinished attempts, slow and stalled, grouped by the first incomplete stage over the selected range. The legend reports only the range maximum because a disappeared stage has no trustworthy current sample. -1 means coverage is unavailable; a healthy empty observer reports zero.', 'normal', ['max']),

  section('Where setup time goes', 300, 29),
  timePanel(
    'Queue wait p95 by controller',
    [prom('histogram_quantile(0.95, sum by (le, name) (rate(workqueue_queue_duration_seconds_bucket{%s}[$__rate_interval])))' % setupQueues, '{{name}}')],
    's', { h: 8, w: 12, x: 0, y: 30 },
    'Time a request waits in each setup controller queue before a worker picks it up. High wait with normal reconcile time means too little worker capacity or a backlog after a restart.',
    noValue='No queued work in range'),
  timePanel(
    'Reconcile time p95 by controller',
    [prom('histogram_quantile(0.95, sum by (le, controller) (rate(controller_runtime_reconcile_time_seconds_bucket{%s}[$__rate_interval])))' % setupReconcilers, '{{controller}}')],
    's', { h: 8, w: 12, x: 12, y: 30 },
    'Time each setup controller spends on one reconcile after dequeue. Rising reconcile time with normal queue wait points at slow Kubernetes API calls or client throttling; see the API section below.',
    noValue='No reconciles in range'),
  timePanel(
    'Reconcile errors by controller',
    [prom('sum by (controller) (rate(controller_runtime_reconcile_errors_total{%s}[$__rate_interval]))' % setupReconcilers, '{{controller}}')],
    'reqps', { h: 8, w: 8, x: 0, y: 38 },
    'Reconciles that returned an error and will retry with backoff. Each retry adds backoff delay to setup.'),
  timePanel(
    'Requeues by controller',
    [prom('sum by (controller, result) (rate(controller_runtime_reconcile_total{%s,result=~"requeue|requeue_after"}[$__rate_interval]))' % setupReconcilers, '{{controller}} {{result}}')],
    'reqps', { h: 8, w: 8, x: 8, y: 38 },
    'Reconciles that asked to run again, either immediately (requeue) or after a delay (requeue_after). Sustained requeues usually mean a controller is waiting on another stage.'),
  timePanel(
    'Queue depth by controller',
    [prom('sum by (name) (workqueue_depth{%s})' % setupQueues, '{{name}}')],
    'short', { h: 8, w: 8, x: 16, y: 38 },
    'Requests waiting for a worker in each setup controller queue.'),
  timePanel(
    'Projection phase p95',
    [prom('(histogram_quantile(0.95, sum by (le, phase) (rate(service_catalog_projection_phase_duration_seconds_bucket[$__rate_interval])))) or label_replace(absent(histogram_quantile(0.95, sum by (le, phase) (rate(service_catalog_projection_phase_duration_seconds_bucket[$__rate_interval])))) * -1, "phase", "no observations", "", "")', '{{phase}}')],
    's', { h: 8, w: 12, x: 0, y: 46 },
    'Execution time for bounded projection phases inside a location-projection reconcile. This does not include workqueue delay.'),
  timePanel(
    'Setup completion volume by class',
    [prom('(sum by (service_class) (increase(service_catalog_setup_attempts_total{outcome="succeeded"}[$__interval]))) or label_replace(absent(sum by (service_class) (increase(service_catalog_setup_attempts_total{outcome="succeeded"}[$__interval]))) * 0, "service_class", "none", "", "")', '{{service_class}}')],
    'short', { h: 8, w: 12, x: 12, y: 46 },
    'Successful setup attempts per interval and service class. Each completion is counted once. Zero means no completion was recorded; observer health shows whether coverage is trustworthy.', 'normal'),

  section('Kubernetes API: slow server or client throttling?', 400, 54),
  statPanel('API round-trip p95', 'histogram_quantile(0.95, sum by (le) (rate(rest_client_request_duration_seconds_bucket{%s}[$__rate_interval]))) or (absent(rest_client_request_duration_seconds_bucket{%s}) * -1)' % [job, job], 'p95', 's',
    [['purple', null], ['green', 0], ['yellow', 0.5], ['red', 2]], { h: 5, w: 6, x: 0, y: 55 },
    'Controller-wide Kubernetes API round-trip latency, excluding time waiting on the client rate limiter. High values mean the API servers are slow. Not exported yet means the deployed release does not register client-go latency metrics.', telemetryMappings('No request observations', clientMetricsPending), 'area'),
  statPanel('Client throttle wait p95', '(histogram_quantile(0.95, sum by (le) (rate(rest_client_rate_limiter_duration_seconds_bucket{%s}[$__rate_interval])))) or (sum(absent(rest_client_rate_limiter_duration_seconds_bucket{%s})) * -1)' % [job, job], 'p95', 's',
    [['purple', null], ['green', 0], ['yellow', 0.1], ['red', 1]], { h: 5, w: 6, x: 6, y: 55 },
    'Controller-wide time requests wait for a client-side rate-limit token before being sent. High values mean the controller is throttling itself, not that the API is slow. Not exported yet must not be read as zero wait.', telemetryMappings('No throttle observations', clientMetricsPending), 'area'),
  statPanel('Request time spent throttled', '(sum(rate(rest_client_rate_limiter_duration_seconds_sum{%s}[$__rate_interval])) / clamp_min(sum(rate(rest_client_rate_limiter_duration_seconds_sum{%s}[$__rate_interval])) + sum(rate(rest_client_request_duration_seconds_sum{%s}[$__rate_interval])), 1e-9)) or (absent(rest_client_rate_limiter_duration_seconds_sum{%s}) * -1)' % [job, job, job, job], 'throttled', 'percentunit',
    [['purple', null], ['green', 0], ['yellow', 0.1], ['red', 0.3]], { h: 5, w: 6, x: 12, y: 55 },
    'Share of total request time spent waiting on the client rate limiter rather than on the API server. Above roughly 10% the controller limits itself; below it, slowness comes from the server.', telemetryMappings('No request observations', clientMetricsPending), 'area'),
  statPanel('HTTP 429 rate', 'sum(rate(rest_client_requests_total{%s,code="429"}[$__rate_interval])) or vector(0)' % job, '429/s', 'reqps',
    [['green', null], ['yellow', 0.01], ['red', 0.1]], { h: 5, w: 6, x: 18, y: 55 },
    'Kubernetes API responses rejected by server-side throttling (API priority and fairness).', [], 'area'),
  timePanel(
    'API round-trip p95 by verb and scope',
    [prom('histogram_quantile(0.95, sum by (le, scope, verb) (rate(rest_client_request_duration_seconds_bucket{%s}[$__rate_interval])))' % job, '{{scope}} {{verb}}')],
    's', { h: 8, w: 12, x: 0, y: 60 },
    'Controller-wide p95 API round-trip latency by scope (root is the platform API, project is any project control plane) and HTTP verb. Client metrics have no reconciler label; compare with Reconcile time p95 by controller.',
    noValue=clientMetricsIdleOrPending % 'latency'),
  timePanel(
    'Client throttle wait p95 by verb and scope',
    [prom('histogram_quantile(0.95, sum by (le, scope, verb) (rate(rest_client_rate_limiter_duration_seconds_bucket{%s}[$__rate_interval])))' % job, '{{scope}} {{verb}}')],
    's', { h: 8, w: 12, x: 12, y: 60 },
    'Controller-wide p95 wait for a client-side rate-limit token by scope (root is the platform API, project is any project control plane) and HTTP verb.',
    noValue=clientMetricsIdleOrPending % 'rate-limiter'),
  timePanel(
    'Kubernetes API request rate by response code',
    [prom('(sum by (code) (rate(rest_client_requests_total{%s}[$__rate_interval]))) or label_replace(absent(sum by (code) (rate(rest_client_requests_total{%s}[$__rate_interval]))) * 0, "code", "none", "", "")' % [job, job], '{{code}}')],
    'reqps', { h: 7, w: 24, x: 0, y: 68 },
    'Controller-wide REST traffic grouped by HTTP response code.', 'normal'),

  section('Leadership, restarts and reconnection', 500, 75),
  statPanel('Leader changes', '(sum(count_over_time((leader_election_master_status{%s} == 1 unless leader_election_master_status{%s} offset 1m == 1)[$__range:1m]))) or vector(0)' % [job, job], 'acquisitions', 'short',
    [['green', null], ['yellow', 1], ['red', 4]], { h: 5, w: 4, x: 0, y: 76 },
    'Times a replica acquired a manager lease in the selected range, across the primary and projection elections. Each change stalls that manager while the new leader starts, syncs caches and reconnects projects.'),
  statPanel('Controller starts', '(count(min_over_time(process_start_time_seconds{%s}[$__range]) > time() - $__range_s) or vector(0)) + (sum(increase(kube_pod_container_status_restarts_total{%s}[$__range])) or vector(0))' % [job, controllerPods], 'starts', 'short',
    [['green', null], ['yellow', 1], ['red', 3]], { h: 5, w: 4, x: 4, y: 76 },
    'Controller processes started in the selected range: new pods from rollouts plus in-place container restarts from crashes and leadership-loss exits. Setup right after a start waits for projects to reconnect.'),
  statPanel('Container restarts', 'sum(increase(kube_pod_container_status_restarts_total{%s}[$__range])) or vector(0)' % controllerPods, 'restarts', 'short',
    [['green', null], ['yellow', 1], ['red', 3]], { h: 5, w: 4, x: 8, y: 76 },
    'Container restarts in place during the selected range. Leadership loss intentionally terminates the process, so restarts usually pair with a leader change.'),
  statPanel('Projects awaiting reconnection', 'sum(workqueue_depth{%s}) or vector(-1)' % projectReconnectQueue, 'queued', 'short',
    [['purple', null], ['green', 0], ['yellow', 1], ['red', 50]], { h: 5, w: 4, x: 12, y: 76 },
    'Project control planes queued for connection. Every project is queued after a start or failover; setup in a project waits until it is reconnected. Should return to zero within seconds.', telemetryMappings('Not reported', 'Not reported'), 'area'),
  statPanel('Projection leaders', 'sum(leader_election_master_status{%s,name=~"(consumer|project)-projection[.]services[.]miloapis[.]com"}) or vector(-1)' % job, 'leaders', 'short',
    [['purple', null], ['red', 0], ['green', 1], ['red', 2]], { h: 5, w: 4, x: 16, y: 76 },
    'Exactly one replica should own the projection lease. -1 means this release has not exposed the distinct projection election.', telemetryMappings('Not reported')),
  statPanel('Primary manager leaders', 'sum(leader_election_master_status{%s,name="services.miloapis.com"}) or vector(-1)' % job, 'leaders', 'short',
    [['purple', null], ['red', 0], ['green', 1], ['red', 2]], { h: 5, w: 4, x: 20, y: 76 },
    'Exactly one primary controller manager leader is expected.', telemetryMappings('Not reported')),
  timePanel(
    'Project reconnection backlog by replica',
    [prom('sum by (pod) (workqueue_depth{%s})' % projectReconnectQueue, '{{pod}}')],
    'short', { h: 8, w: 12, x: 0, y: 81 },
    'Projects still waiting to be reconnected on each replica. After a start the backlog jumps to every project and drains; the time to drain is the time to reconnect projects after a restart.'),
  timePanel(
    'Lease holder by manager',
    [prom('max by (name, pod) (leader_election_master_status{%s}) == 1' % job, '{{name}} on {{pod}}')],
    'short', { h: 8, w: 12, x: 12, y: 81 },
    'Which replica holds each manager lease. A line ending and another starting is a leader change.',
    legendCalcs=[], noValue='No replica holds a lease'),

  collapsedSection('Observer and scrape health', 600, 89, [
  statPanel('Last successful observation', '(time() - max(service_catalog_setup_observer_last_success_timestamp_seconds)) or vector(-1)', 'age', 's',
    [['purple', null], ['green', 0], ['yellow', 120], ['red', 300]], { h: 5, w: 8, x: 0, y: 90 },
    'Seconds since the aggregate writer last completed a successful observation pass. -1 means it has never reported.', telemetryMappings('Never reported'), 'area'),
  statPanel(
    'Replicas scraped',
    '(count(up{%s} == 1) / clamp_min(count(up{%s}), 1)) or vector(0)' % [job, job],
    'scrape coverage', 'percentunit', [['red', null], ['yellow', 0.5], ['green', 1]],
    { h: 5, w: 8, x: 8, y: 90 },
    'Fraction of discovered service-catalog controller targets that VictoriaMetrics is scraping successfully.', [], 'area'),
  statPanel('Controller targets up', 'count(up{%s} == 1) or vector(0)' % job, 'up', 'short',
    [['red', null], ['yellow', 1], ['green', 2]], { h: 5, w: 8, x: 16, y: 90 },
    'Number of service-catalog controller targets currently scraped as up. This proves scrape reachability, not cache completeness.', [], 'area'),
  ]),

  collapsedSection('Rollout and version', 700, 90, [
  statPanel('Ready controller pods', 'sum(kube_pod_status_ready{namespace="services-system",pod=~"services-controller-manager-.*",condition="true"}) or vector(0)', 'ready', 'short',
    [['red', null], ['yellow', 1], ['green', 2]], { h: 5, w: 12, x: 0, y: 91 },
    'Ready service-catalog controller-manager pods.'),
  statPanel('Process age (youngest)', 'min(time() - process_start_time_seconds{%s}) or vector(-1)' % job, 'age', 's',
    [['purple', null], ['red', 0], ['yellow', 300], ['green', 1800]], { h: 5, w: 12, x: 12, y: 91 },
    'Age of the youngest scraped controller process. A reset identifies rollout or restart timing.', telemetryMappings('Not reported'), 'area'),
  timePanel(
    'Replica count by image over selected range',
    [prom('(count by (image) (kube_pod_container_info{%s})) or label_replace(absent(count by (image) (kube_pod_container_info{%s})) * 0, "image", "not observed", "", "")' % [controllerPods, controllerPods], '{{image}}')],
    'short', { h: 8, w: 24, x: 0, y: 96 },
    'Replica count by immutable image reference over the selected range. Historical image series remain visible to show rollout transitions.'),
  ]),
];

// Controller starts and lease acquisitions on every time series: they cause
// most setup latency outliers.
local annotations = [
  {
    builtIn: 1,
    datasource: { type: 'grafana', uid: '-- Grafana --' },
    enable: true,
    hide: true,
    iconColor: 'rgba(0, 211, 255, 1)',
    name: 'Annotations & Alerts',
    type: 'dashboard',
  },
  {
    datasource: { type: 'prometheus', uid: ds },
    enable: true,
    expr: '(time() - max by (pod) (process_start_time_seconds{%s})) < 60' % job,
    iconColor: 'orange',
    name: 'Controller starts',
    step: '30s',
    titleFormat: 'Controller started',
    textFormat: '{{pod}}',
  },
  {
    datasource: { type: 'prometheus', uid: ds },
    enable: true,
    expr: 'max by (name, pod) (leader_election_master_status{%s} == 1 unless leader_election_master_status{%s} offset 1m == 1)' % [job, job],
    iconColor: 'purple',
    name: 'Leader changes',
    step: '30s',
    titleFormat: 'Lease acquired',
    textFormat: '{{name}} on {{pod}}',
  },
];

g.dashboard.new('Service Catalog Setup')
+ g.dashboard.withUid('service-catalog-setup')
+ g.dashboard.withDescription('Is service setup fast and healthy right now, and if not, why: slow versus stalled setups, where setup time goes, API latency versus client throttling, and the leader changes and restarts behind outliers.')
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
+ { annotations: { list: annotations } }
+ g.dashboard.withPanels(panels)
