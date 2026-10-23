# Service Catalog observability

The setup dashboard follows the Jsonnet and Grafonnet layout used by
[milo-os/activity](https://github.com/milo-os/activity/tree/main/observability).
It answers "is setup fast and healthy right now, and if not, why?": setup
that is slow right now versus stalled on a known blocker, per-controller
queue wait and reconcile time, Kubernetes API latency versus client
throttling, and the leader changes, restarts and project reconnection behind
most outliers.

Install Python 3 (available as `python3`), `jsonnet`, and `jb`, then run:

```sh
task observability:install
task observability:build
task observability:verify
```

The generated dashboard is committed so the observability Kustomize component
can be consumed without a Jsonnet toolchain. Empty results use semantic states
such as `Telemetry unavailable`, `No observations`, and `No completed setups`;
panels must not use Grafana's generic `No data` message.

## Alerts

`config/components/observability/alerts` ships a `PrometheusRule` with the
setup alerts, so any environment that includes the observability component
gets them alongside the dashboard:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `ServiceActivationSlow` | p95 activation time over 30 minutes, measured from authorization, exceeds 15 seconds (at least 3 completions) | warning |
| `ServiceActivationOverdue` | An activation is more than a minute past authorization but not yet stalled | warning |
| `ServiceActivationStalled` | An activation is more than an hour past authorization | warning |
| `ServiceCatalogSetupObserverDown` | No operator pod has a synced setup observer for 10 minutes, so no activation can complete | critical |
| `ServiceCatalogClientThrottled` | p95 wait on the operator's client-side rate limiter exceeds 250ms for 15 minutes | warning |

Install `promtool` and [yq](https://github.com/mikefarah/yq) v4, then run
`task observability:test-alerts` to unit test the
rules. Each alert links to the [service setup runbook](../docs/runbooks/service-setup.md).
