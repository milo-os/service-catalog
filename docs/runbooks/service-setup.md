# Service setup runbook

This runbook covers the service setup alerts in
`config/components/observability/alerts` and the
Service Catalog Setup dashboard (Grafana folder "Platform / Services", UID
`service-catalog-setup`). It is written for
the on-call engineer who received one of these alerts.

Service setup (also called activation) starts when a consumer enables a service
in a project and ends when the project's `ServiceEntitlement` reports
`SetupComplete=True`. The services operator (`services-controller-manager` in
`services-system`) drives setup through five stages, in order:

1. `Authorization`: access is approved. Time waiting for a human approval is
   excluded from every alert.
2. `Dependencies`: every service this service depends on is enabled.
3. `Provisioning`: the resources the service declares are installed in the
   project.
4. `Projection`: the service's `ServiceConfiguration` is projected into the
   project.
5. `ProviderAcknowledgement`: the service provider acknowledges the setup.

## Find the setup that is slow or stuck

Each `ServiceEntitlement` records its setup in `status.setup`. The first stage
in `status.setup.stages` that is not `True` is the blocker, and its `reason`
and `message` say why. Against the project's control plane, run:

```sh
kubectl get serviceentitlements \
  -o custom-columns='NAME:.metadata.name,SERVICE:.spec.serviceRef.name,AUTHORIZED:.status.setup.authorizedAt,COMPLETED:.status.setup.firstCompletedAt'
kubectl get serviceentitlement <name> -o jsonpath='{.status.setup.stages}' | jq
```

The alert's `blocking_stage` and `service_class` labels narrow the search. The
`service_class` label groups services by name: `compute`, `networking`, `dns`,
`storage`, or `other`.

## ServiceActivationSlow

**Meaning:** over the last 30 minutes, the 95th percentile of activation time,
measured from authorization to `SetupComplete`, exceeded 15 seconds for one
service class.

**Diagnose:** on the setup dashboard, compare these panels:

- **Client throttle wait p95** and **Request time spent throttled**: if these are
  high, the operator is waiting on its own client-side rate limiter. See
  [ServiceCatalogClientThrottled](#servicecatalogclientthrottled).
- **API round-trip p95**: if Milo API latency is high, look at the Milo API
  server (memory pressure, restarts) through the **Milo API latency** link.
- **Queue wait p95 by controller** and **Projects awaiting reconnection**: a
  long queue wait right after a controller start or lease change means the
  operator is still reconnecting to every project. That backlog drains on its
  own; if it recurs, investigate why the operator restarted (see **Leader
  changes** and **Container restarts**).

## ServiceActivationOverdue

**Meaning:** at least one activation is unfinished more than one minute after
authorization, but less than one hour. The completion histogram cannot show
this case, because it only records setups that finish.

**Diagnose:** find the entitlement as described above and read the blocking
stage's `reason` and `message`. A setup that is still progressing usually
finishes on its own; check the same causes as for
[ServiceActivationSlow](#serviceactivationslow). If the alert persists for most
of an hour, treat it as stalled.

## ServiceActivationStalled

**Meaning:** at least one activation is unfinished more than one hour after
authorization. Retries will not fix it; something must change first.

**Common causes, by blocking stage:**

- `Projection` with reason `ConfigurationUnavailable`: the service has no
  `ServiceConfiguration`. Ask the service's owner to publish one, or have the
  consumer disable the service.
- `Dependencies`: a dependency could not be enabled. The stage message names
  it; fix that dependency's setup first.
- `Provisioning`: a declared resource failed to install. The entitlement's
  `status.provisionedResources` names the resource and the error.

Once the blocker is fixed, the operator completes setup without further action.
If the consumer no longer needs the service, deleting the entitlement removes
it from the count.

## ServiceCatalogSetupObserverDown

**Meaning:** for 10 minutes, no operator pod has reported a healthy setup
observer, or the operator exports no metrics at all. No activation can complete
while this alert fires.

**Diagnose:**

1. Check the pods (the **Ready controller pods** and **Setup observer** panels
   show the same at a glance): `kubectl -n services-system get pods -l control-plane=services`.
   Look for crash loops, OOM kills, and pending pods.
2. Check the operator logs for leader election loss, TLS or connection timeouts
   to Milo, and `cache sync` errors.
3. Check the Milo API server's health. When Milo is unavailable or restarting,
   the operator cannot sync its caches.

After a restart, the new leader reconnects to every project before setup
resumes. With hundreds of projects this takes several minutes, which is why the
alert waits 10 minutes.

## ServiceCatalogClientThrottled

**Meaning:** for 15 minutes, the operator's Milo API requests have waited a
95th percentile of more than 250ms on its own client-side rate limiter. Milo's
API priority and fairness exempts the operator, so this limiter is the only
throttle on its traffic.

**Fix:** raise the operator's `--kube-api-qps` and `--kube-api-burst` flags
(defaults 50 and 100) in its deployment. If request volume rose suddenly, find
what drives it first: **Queue depth by controller** and **Requeues by
controller** show which controller is busy.
