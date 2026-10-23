# Service Catalog observability

The setup dashboard follows the Jsonnet and Grafonnet layout used by
[milo-os/activity](https://github.com/milo-os/activity/tree/main/observability).
It answers "is setup fast and healthy right now, and if not, why?": setup
that is slow right now versus stalled on a known blocker, per-controller
queue wait and reconcile time, Kubernetes API latency versus client
throttling, and the leader changes, restarts and project reconnection behind
most outliers.

The adoption dashboard answers the product questions: which services projects
use, how adoption changes over time, and how well each service activates
(success, rejections, time to activate, and work in progress). Its metrics are
labelled by service and never by project, so their series count grows with the
catalog, not with the number of projects. `verify_dashboard.py` runs its
generic checks (semantic empty states, unique titles) on both dashboards; its
panel-specific assertions cover only the setup dashboard.

Install Python 3 (available as `python3`), `jsonnet`, and `jb`, then run:

```sh
task observability:install
task observability:build
task observability:verify
```

The generated dashboards are committed so the observability Kustomize component
can be consumed without a Jsonnet toolchain. Empty results use semantic states
such as `Telemetry unavailable`, `No observations`, and `No completed setups`;
panels must not use Grafana's generic `No data` message.
