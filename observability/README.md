# Service Catalog observability

The setup dashboard follows the Jsonnet and Grafonnet layout used by
[milo-os/activity](https://github.com/milo-os/activity/tree/main/observability).
It covers customer-visible setup latency, unfinished work, projection queue
capacity, observer health, leader ownership, Kubernetes API pressure, and
rollout state.

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
