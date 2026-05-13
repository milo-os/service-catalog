# Service Infrastructure

Milo is a multi-tenant platform that lets service providers offer managed services to their consumers. Service infrastructure is the Milo layer that holds the shared governance catalogs those providers (and the signal pipelines that follow their services) depend on — the declarative records of which services exist, which Kubernetes Kinds are billable, quotable, or monitored, and which descriptive labels those Kinds are allowed to carry. It is the source of truth every downstream signal pipeline reads from, rather than re-inventing its own answer.

The model is borrowed loosely from GCP's Service Infrastructure layer, where service management, service config, and monitored resource descriptors sit below Cloud Billing, Cloud Monitoring, and Cloud Logging and feed them all. The Milo equivalent plays the same role: one neutral place that says "this is what exists on the platform," consumed symmetrically by billing, quota, telemetry, and audit.

## Why it matters

Three problems this service is built to solve:

- **One catalog, not three.** Without a shared declaration of which Kinds are billable, quotable, or monitored, every downstream team invents its own list. Finance's spreadsheet, the quota service's registry, and the monitoring team's dashboard config all drift from each other silently, and a new resource type gets added to some but not others. A single catalog — owned by neither billing nor quota — keeps them aligned.
- **The right dependency direction.** If billing owned the catalog of monitored resource types, quota and telemetry would have to depend on billing to learn what counts as a tracked resource. That is backwards: billing, quota, and monitoring are peers. Putting the catalog in a neutral infrastructure layer lets every signal pipeline depend on the same upstream source without cross-dependencies between the signals themselves.
- **A single door for service providers.** A provider publishing a service — the platform's own teams, partners, or channel resellers — needs one place to declare the service's identity and the resource types it exposes to consumers. Service infrastructure is that place. Everything downstream (pricing, quota tracking, usage dashboards, audit lineage) reads from the registration, so the provider does the work once.

## What lives here

Today, three resources are documented:

- **`Service`** — the service registry. A first-class record for each platform service: its canonical name (e.g. `compute.miloapis.com`), its owner, its public-facing description, and its lifecycle state. The identity that every governance CRD's `spec.owner.service` field references. See [`docs/enhancements/service-registry.md`](docs/enhancements/service-registry.md).
- **`MeterDefinition`** — declares a billable dimension: what is measured, in what unit, how it's aggregated, and how it crosses into commerce. Consumed by billing (today) and by any future system that needs a stable measurement catalog (quota showback, capacity planning, partner SKUs). See [`docs/enhancements/metering-definitions.md`](docs/enhancements/metering-definitions.md).
- **`MonitoredResourceType`** — declares which Kubernetes Kinds are platform-governed resources, who owns them, and the closed set of descriptive labels that events or counters about them may carry. See [`docs/enhancements/monitored-resource-types.md`](docs/enhancements/monitored-resource-types.md).

Near-term additions expected to land here:

- **Adjacent governance resources** as they emerge: service-level descriptors, API surface metadata, and any additional type-catalog needs that downstream services identify.

The common shape across everything that lives here: declarative, type-level, measurement-agnostic, and consumed by multiple signal pipelines rather than any single one.

## How other services use this

Service infrastructure is consumed, not called into at runtime for user traffic.

**Downstream push.** When the services operator is running it acts as the authoritative
writer for billing's own `MeterDefinition` and `MonitoredResourceType` types. On every
reconcile it server-side-applies the billing representation, sets ownership labels, and
attaches an owner reference so Kubernetes GC cascades deletes automatically. This means
billing and quota are independently deployable — they define their own CRD types and
can be managed directly without the services operator present.

```
Provider publishes services.MeterDefinition
        │
        ▼  services operator pushes
billing.MeterDefinition  (owned by services operator via label + owner ref)
        │
        ▼  providers (e.g. amberflo-provider) reconcile billing type
   Downstream API
```

Known consumers:

- **[billing](https://github.com/milo-os/billing)** — the services operator pushes
  `MeterDefinition` and `MonitoredResourceType` into billing's representations of those
  types. Providers like `amberflo-provider` watch billing types only and have no
  compile-time dependency on this repo.
- **The future quota service** will read the same catalog to know which Kinds it is
  entitled to count against project and account quotas.
- **Future telemetry and audit pipelines** will read the catalog to attach stable,
  human-readable resource-type names to every signal, dashboard drill-down, and audit
  record.

Each consumer joins on the canonical resource-type name, so a quota decision, a billing
event, and an audit record all refer to the same object by the same identifier.

See [docs/enhancements/downstream-push-architecture.md](docs/enhancements/downstream-push-architecture.md)
for the full design.

## Non-goals

To be clear about what service infrastructure is *not*:

- **Not a runtime.** It does not serve user traffic, route requests, or process events. It is a declarative catalog.
- **Not a billing system.** It does not hold prices, meter definitions, or usage data. Billing resources (`MeterDefinition`, `BillingAccount`, the usage pipeline) live in the billing service.
- **Not a telemetry pipeline.** It declares which resource types are monitored; it does not collect, store, or query metrics or logs.
- **Not an identity, RBAC, or entitlement system.** It describes what exists on the platform, not who may access it.

## Development

Prerequisites: Go 1.24+, [Task](https://taskfile.dev), and a Kubernetes cluster (local or remote).

```bash
# Build the binary
task build

# Run tests
task test

# Run the linter
task lint

# Regenerate CRD manifests and RBAC
task manifests

# Regenerate deepcopy and other code
task generate
```

The repo uses a Go workspace (`go.work`) that includes the `billing` and `amberflo-provider` sibling modules so gopls and `go build` resolve cross-module references without needing published releases.

## Related repos

- **[milo-os/billing](https://github.com/milo-os/billing)** — billing service; consumes `MeterDefinition` and `MonitoredResourceType` pushed by this operator.
- **[milo-os/compute](https://github.com/milo-os/compute)** — compute service; one of the platform services registered here.
- **[milo-os/galactic](https://github.com/milo-os/galactic)** — networking service; VPC, everywhere.
- **[milo-os/activity](https://github.com/milo-os/activity)** — human-readable activity timelines from control plane audit events.

## Further reading

- [`docs/enhancements/`](docs/enhancements/) — enhancement proposals for each resource hosted here. The canonical place to go for design detail, spec shape, and worked examples.
