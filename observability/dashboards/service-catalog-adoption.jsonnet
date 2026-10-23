// Service Catalog Adoption Dashboard
//
// Answers the product questions the setup dashboard does not: which services
// projects use, how that changes over time, and how well each service
// activates. Every metric here is labelled by service, never by project, so
// the series count grows with the catalog rather than with the number of
// projects. Entitlement counts come from the projection owner's in-memory
// snapshot; after a controller start they climb back to their true value as
// project control planes reconnect, which the Setup observer stat reports.

local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet';

local prometheus = g.query.prometheus;
local stat = g.panel.stat;
local timeSeries = g.panel.timeSeries;
local row = g.panel.row;
local table = g.panel.table;
local var = g.dashboard.variable;

local ds = '${datasource}';
local svc = 'service=~"$service"';
local entitlements(selector) = 'sum by (service) (service_catalog_service_entitlements{%s,%s})' % [svc, selector];

local prom(expr, legend='') =
  prometheus.new(ds, expr)
  + prometheus.withLegendFormat(legend);

local instant(expr, refId) =
  prom(expr)
  + prometheus.withInstant(true)
  + prometheus.withFormat('table')
  + prometheus.withRefId(refId);

local pos(h, w, x, y) = { gridPos: { h: h, w: w, x: x, y: y } };
local steps(values) = [{ color: value[0], value: value[1] } for value in values];

local statPanel(title, expr, unit, thresholds, gridPos, description, noValue='No observations', graphMode='none') =
  stat.new(title)
  + pos(gridPos.h, gridPos.w, gridPos.x, gridPos.y)
  + stat.panelOptions.withDescription(description)
  + stat.queryOptions.withDatasource('prometheus', ds)
  + stat.queryOptions.withTargets([prom(expr)])
  + stat.options.withColorMode('background')
  + stat.options.withGraphMode(graphMode)
  + stat.options.withTextMode('auto')
  + stat.options.reduceOptions.withCalcs(['lastNotNull'])
  + stat.standardOptions.withUnit(unit)
  + stat.standardOptions.withNoValue(noValue)
  + stat.standardOptions.color.withMode('thresholds')
  + stat.standardOptions.thresholds.withMode('absolute')
  + stat.standardOptions.thresholds.withSteps(steps(thresholds));

local timePanel(title, targets, unit, gridPos, description, stacking='none', noValue='No observations in range') =
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
  + timeSeries.options.legend.withCalcs(['lastNotNull', 'max'])
  + timeSeries.options.legend.withSortBy('Max')
  + timeSeries.options.legend.withSortDesc(true);

// One row per service, built from instant queries merged on the service label.
local serviceTable(title, gridPos, description, targets, columns, overrides, sortBy) =
  table.new(title)
  + pos(gridPos.h, gridPos.w, gridPos.x, gridPos.y)
  + table.panelOptions.withDescription(description)
  + table.queryOptions.withDatasource('prometheus', ds)
  + table.queryOptions.withTargets(targets)
  + table.queryOptions.withTransformations([
    { id: 'merge', options: {} },
    { id: 'organize', options: {
      excludeByName: { Time: true },
      indexByName: { service: 0 } + { ['Value #' + columns[i][0]]: i + 1 for i in std.range(0, std.length(columns) - 1) },
      renameByName: { service: 'Service' } + { ['Value #' + column[0]]: column[1] for column in columns },
    } },
  ])
  + table.standardOptions.withNoValue('No services in use')
  + table.standardOptions.withOverrides(overrides)
  + table.options.withSortBy([{ displayName: sortBy, desc: true }]);

local section(title, id, y) =
  row.new(title)
  + row.withId(id)
  + pos(1, 24, 0, y)
  + row.withCollapsed(false);

local datasourceVar =
  var.datasource.new('datasource', 'prometheus')
  + var.datasource.generalOptions.withLabel('Data Source')
  + var.datasource.generalOptions.withCurrent('Service Metrics', 'SERVICE_METRICS')
  + var.datasource.generalOptions.showOnDashboard.withLabelAndValue()
  + var.datasource.withRegex('');

local serviceVar =
  var.query.new('service', 'label_values(service_catalog_service_entitlements, service)')
  + var.query.withDatasource('prometheus', ds)
  + var.query.generalOptions.withLabel('Service')
  + var.query.selectionOptions.withMulti(true)
  + var.query.selectionOptions.withIncludeAll(true, '.*')
  + var.query.refresh.onTime()
  + var.query.withSort(1);

local panels = [
  section('Who uses which services?', 100, 0),
  statPanel('Active entitlements', 'sum(service_catalog_service_entitlements{%s,phase="Active"})' % svc, 'short',
    [['blue', null]], { h: 4, w: 4, x: 0, y: 1 },
    'Active ServiceEntitlements across all projects, including ones a service pulled in as a dependency. A project with three services counts three times; projects are deliberately not a metric label.',
    'Not reported', 'area'),
  statPanel('Services in use', 'count(%s > 0)' % entitlements('phase="Active"'), 'short',
    [['blue', null]], { h: 4, w: 4, x: 4, y: 1 },
    'Catalog services with at least one Active entitlement.', 'Not reported'),
  statPanel('Awaiting provider approval', 'sum(service_catalog_service_entitlements{%s,phase="PendingApproval"}) or vector(0)' % svc, 'short',
    [['green', null], ['yellow', 1], ['orange', 10]], { h: 4, w: 4, x: 8, y: 1 },
    'Entitlements for gated services that are waiting on the provider to approve or deny them.', 'Not reported'),
  statPanel('Activated in range', 'sum(increase(service_catalog_service_activations_total{%s,outcome="succeeded"}[$__range]))' % svc, 'short',
    [['green', null]], { h: 4, w: 4, x: 12, y: 1 },
    'Entitlements that finished setup for the first time in the selected range.'),
  statPanel('Disabled in range', 'sum(increase(service_catalog_service_disables_total{%s,origin="Direct"}[$__range]))' % svc, 'short',
    [['text', null]], { h: 4, w: 4, x: 16, y: 1 },
    'Entitlements a project removed in the selected range. Dependencies removed along with their parent are excluded.'),
  statPanel('Rejected in range', 'sum(increase(service_catalog_service_activations_total{%s,outcome="rejected"}[$__range]))' % svc, 'short',
    [['green', null], ['orange', 1]], { h: 4, w: 4, x: 20, y: 1 },
    'Entitlements that moved to Rejected in the selected range: a provider denial, or a request for a service that is not published.'),
  serviceTable(
    'Adoption by service', { h: 9, w: 24, x: 0, y: 5 },
    'Current entitlements per service, split into ones a project asked for directly and ones another service pulled in as a dependency, plus enables, disables, and rejections over the selected range. "unknown" collects requests for names that do not match a catalog Service.',
    [
      instant(entitlements('phase="Active",origin="Direct"'), 'direct'),
      instant(entitlements('phase="Active",origin="Dependency"'), 'dependency'),
      instant(entitlements('phase="PendingApproval"'), 'pending'),
      instant('sum by (service) (increase(service_catalog_service_activations_total{%s,outcome="succeeded"}[$__range]))' % svc, 'activated'),
      instant('sum by (service) (increase(service_catalog_service_disables_total{%s,origin="Direct"}[$__range]))' % svc, 'disabled'),
      instant('sum by (service) (increase(service_catalog_service_activations_total{%s,outcome="rejected"}[$__range]))' % svc, 'rejected'),
    ],
    [['direct', 'Active (direct)'], ['dependency', 'Active (dependency)'], ['pending', 'Awaiting approval'], ['activated', 'Activated'], ['disabled', 'Disabled'], ['rejected', 'Rejected']],
    [{ matcher: { id: 'byRegexp', options: 'Activated|Disabled|Rejected' }, properties: [{ id: 'decimals', value: 0 }] }],
    'Active (direct)'),
  timePanel('Active entitlements by service',
    [prom(entitlements('phase="Active"'), '{{service}}')],
    'short', { h: 8, w: 12, x: 0, y: 14 },
    'Adoption over time. A dip right after a controller start is the snapshot refilling as projects reconnect, not churn; compare with Disables per hour.', 'normal'),
  timePanel('Activations and disables per hour',
    [
      prom('sum by (service) (increase(service_catalog_service_activations_total{%s,outcome="succeeded"}[1h]))' % svc, '{{service}} activated'),
      prom('-sum by (service) (increase(service_catalog_service_disables_total{%s,origin="Direct"}[1h]))' % svc, '{{service}} disabled'),
    ],
    'short', { h: 8, w: 12, x: 12, y: 14 },
    'New activations above the line and disables below it, over a rolling hour. Net growth is the difference.'),

  section('How well does each service activate?', 200, 22),
  statPanel('Activations under 1s',
    'sum(increase(service_catalog_service_setup_duration_seconds_bucket{%s,clock="user",le="1"}[$__range])) / sum(increase(service_catalog_service_setup_duration_seconds_count{%s,clock="user"}[$__range]))' % [svc, svc],
    'percentunit', [['red', null], ['yellow', 0.5], ['green', 0.9]], { h: 4, w: 6, x: 0, y: 23 },
    'Share of activations in the selected range that finished within one second of the request, the project objective.', 'No completed setups'),
  statPanel('Activation p95',
    'histogram_quantile(0.95, sum by (le) (increase(service_catalog_service_setup_duration_seconds_bucket{%s,clock="user"}[$__range])))' % svc,
    's', [['green', null], ['yellow', 1], ['red', 10]], { h: 4, w: 6, x: 6, y: 23 },
    'Request to first successful setup, 95th percentile over the selected range.', 'No completed setups'),
  statPanel('In progress', 'sum(service_catalog_service_setup_inflight{%s}) or vector(0)' % svc, 'short',
    [['green', null], ['yellow', 5]], { h: 4, w: 6, x: 12, y: 23 },
    'Entitlements whose first setup has not finished yet, including ones waiting on provider approval.', 'Not reported'),
  statPanel('Stalled (>1h)', 'sum(service_catalog_service_setup_stalled{%s}) or vector(0)' % svc, 'short',
    [['green', null], ['orange', 1]], { h: 4, w: 6, x: 18, y: 23 },
    'Setups more than an hour past authorization. The Service Catalog Setup dashboard breaks these down by blocking stage.', 'Not reported'),
  serviceTable(
    'Activation by service', { h: 9, w: 24, x: 0, y: 27 },
    'Per service: how many activations finished and how long they took over the selected range, how many are still in progress, and how many are stuck. User time runs from the request; platform time runs from authorization, so it excludes waiting for provider approval.',
    [
      instant('sum by (service) (increase(service_catalog_service_setup_duration_seconds_count{%s,clock="user"}[$__range]))' % svc, 'completed'),
      instant('sum by (service) (increase(service_catalog_service_setup_duration_seconds_bucket{%s,clock="user",le="1"}[$__range])) / sum by (service) (increase(service_catalog_service_setup_duration_seconds_count{%s,clock="user"}[$__range]))' % [svc, svc], 'under1s'),
      instant('histogram_quantile(0.5, sum by (le, service) (increase(service_catalog_service_setup_duration_seconds_bucket{%s,clock="user"}[$__range])))' % svc, 'p50'),
      instant('histogram_quantile(0.95, sum by (le, service) (increase(service_catalog_service_setup_duration_seconds_bucket{%s,clock="user"}[$__range])))' % svc, 'p95'),
      instant('histogram_quantile(0.95, sum by (le, service) (increase(service_catalog_service_setup_duration_seconds_bucket{%s,clock="platform"}[$__range])))' % svc, 'platform'),
      instant('sum by (service) (service_catalog_service_setup_inflight{%s})' % svc, 'inflight'),
      instant('sum by (service) (service_catalog_service_setup_stalled{%s})' % svc, 'stalled'),
    ],
    [['completed', 'Completed'], ['under1s', 'Under 1s'], ['p50', 'p50'], ['p95', 'p95'], ['platform', 'Platform p95'], ['inflight', 'In progress'], ['stalled', 'Stalled']],
    [
      { matcher: { id: 'byName', options: 'Completed' }, properties: [{ id: 'decimals', value: 0 }] },
      { matcher: { id: 'byName', options: 'Under 1s' }, properties: [{ id: 'unit', value: 'percentunit' }, { id: 'decimals', value: 0 }] },
      { matcher: { id: 'byRegexp', options: 'p50|p95|Platform p95' }, properties: [{ id: 'unit', value: 's' }] },
    ],
    'Completed'),
  timePanel('Activation p95 by service',
    [prom('histogram_quantile(0.95, sum by (le, service) (rate(service_catalog_service_setup_duration_seconds_bucket{%s,clock="user"}[$__rate_interval])))' % svc, '{{service}}')],
    's', { h: 8, w: 12, x: 0, y: 36 },
    'Request to first successful setup over time. Gaps are periods with no completions for that service.'),
  timePanel('In-progress setups by service and stage',
    [prom('sum by (service, blocking_stage) (service_catalog_service_setup_inflight{%s})' % svc, '{{service}} at {{blocking_stage}}')],
    'short', { h: 8, w: 12, x: 12, y: 36 },
    'Unfinished first setups, by service and the first stage that has not completed. Authorization includes waiting on provider approval.', 'normal', 'Nothing in progress'),
];

g.dashboard.new('Service Catalog Adoption')
+ g.dashboard.withUid('service-catalog-adoption')
+ g.dashboard.withDescription('Which services projects use, how that changes over time, and how well each service activates.')
+ g.dashboard.withTags(['milo', 'service-catalog', 'adoption', 'product'])
+ g.dashboard.withEditable(true)
+ g.dashboard.withRefresh('1m')
+ g.dashboard.withSchemaVersion(39)
+ g.dashboard.graphTooltip.withSharedCrosshair()
+ g.dashboard.time.withFrom('now-7d')
+ g.dashboard.time.withTo('now')
+ g.dashboard.withLinks([
  { icon: 'dashboard', includeVars: false, keepTime: true, targetBlank: false, title: 'Service Catalog Setup', tooltip: 'Operator view: slow and stalled setups and why', type: 'link', url: '/d/service-catalog-setup' },
])
+ g.dashboard.withVariables([datasourceVar, serviceVar])
+ g.dashboard.withPanels(panels)
