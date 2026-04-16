# Plan: `ServiceConfiguration` as the single provider-facing CRD

## Scope

This plan covers **only** what the `billing` service currently consumes.
Future consumers (quota, entitlements, SLO, marketplace) are explicit
non-goals; when they land they will get their own top-level sections on
`ServiceConfiguration` and their own fan-out reconcilers.

## Context

Today, providers configure a service on Milo by creating three separate
cluster-scoped resources: `Service`, `MeterDefinition`, and
`MonitoredResourceType`. Downstream services read or fan out from these.
Problems:

1. **Fragmented authoring UX** — configuring a service takes a bundle of
   related-but-separate kubectl applies. Cross-resource references
   (meter → service, monitored-resource-type → service) are enforced at
   admission and re-checked at reconciliation, with race windows.
2. **Duplication across downstream consumers** — each consumer wants a
   differently shaped type. Forcing them all onto shared `services.*`
   types couples their schema evolution together.

## What `billing` actually consumes

From `/Users/scotwells/repos/datum-cloud/billing/api/v1alpha1`:

- `billing.miloapis.com/MeterDefinition` — `meterName`, `phase`,
  `displayName`, `description`, `measurement{aggregation, unit,
  dimensions}`, `billing{consumedUnit, pricingUnit}`, and (to be added)
  `monitoredResourceTypes: []string` linking the meter to the resource
  types that emit it. Ownership expressed via the
  `services.miloapis.com/service` label (no `spec.owner` field).
- `billing.miloapis.com/MonitoredResourceType` — `resourceTypeName`,
  `phase`, `displayName`, `description`, `gvk{group, kind}`, `labels[]`.
  Same ownership-via-label convention.

Billing has no "destination" concept (single sink today) so the routing
layer from Google's pattern (`BillingDestination.monitored_resource +
metrics`) is not needed. The per-metric `monitored_resource_types` field
(Google's canonical metric→resource link) is needed — added both to
`ServiceConfiguration.spec.meters[]` and to billing's `MeterDefinition`
so the fan-out has a place to land it.

`consumedUnit`/`pricingUnit` are per-meter in billing's schema, so they
must live per-meter in `ServiceConfiguration` as well.

## Architecture

- **One user-facing CRD**: `services.miloapis.com/v1alpha1/ServiceConfiguration` —
  flat, Google Service Infrastructure-style layout (primitives at top
  level, consumer sections reference them by name when they appear).
- **`Service` CRD kept** as the identity record, referenced by
  `ServiceConfiguration.spec.serviceRef`. 1:1 for now; no rollout/history.
- **Services operator fans out** `ServiceConfiguration` into billing's
  two CRDs via server-side apply, with owner refs back to the
  `ServiceConfiguration`.
- **Billing CRDs stay in the billing repo**; services operator imports
  `go.miloapis.com/billing/api/v1alpha1` as a compile-time dependency
  (as it already does).

## Proposed type shape

```go
type ServiceConfigurationSpec struct {
    ServiceRef ServiceReference        // -> Service CRD by metadata.name
    Phase      Phase                   // service-level lifecycle

    MonitoredResourceTypes []MonitoredResourceType
    Meters                 []Meter
    // Future consumers (quota, entitlements, ...) add top-level sections
    // here; out of scope for this plan.
}

type Meter struct {
    Name                     string           // canonical, e.g. compute.miloapis.com/instance/cpu-seconds
    DisplayName, Description string
    Measurement              MeterMeasurement // aggregation, unit, dimensions
    Billing                  MeterBilling     // consumedUnit, pricingUnit
    MonitoredResourceTypes   []string         // required, min 1; references spec.monitoredResourceTypes[].type
}

type MonitoredResourceType struct {
    Type                     string           // canonical, e.g. compute.miloapis.com/Instance
    DisplayName, Description string
    GVK                      GVKRef           // group + kind
    Labels                   []MonitoredResourceLabel
}
```

Name uniqueness is now within a document, not cluster-wide → the
admission-race concern on `meterName` / `resourceTypeName` disappears
naturally.

Canonical names on meters and monitored resource types must still be
prefixed by the referenced service's `spec.serviceName` — the webhook
enforces this by resolving `serviceRef`.

## Fan-out to billing

One `ServiceConfiguration` reconcile produces, per managed entry:

- One `billing.miloapis.com/MeterDefinition` per element of
  `spec.meters`, SSA'd with:
  - `metadata.name` = deterministic encoding of `meter.name`
  - `labels["services.miloapis.com/service"]` = service's canonical name
  - `labels["app.kubernetes.io/managed-by"]` = `services-operator`
  - Owner ref back to the `ServiceConfiguration`
  - Spec field-by-field translation, including
    `monitoredResourceTypes` copied through verbatim
- One `billing.miloapis.com/MonitoredResourceType` per element of
  `spec.monitoredResourceTypes`, same convention.

Removals: on each reconcile, list billing objects with the
`managed-by=services-operator` + owner-ref-matches-this-ServiceConfig
selector and delete any not in the current desired set. Owner-ref
cascade handles the `ServiceConfiguration` delete case.

Drafts: elements with an effective phase of `Draft` are skipped on the
fan-out (matching the current `billingmeterdefinition_controller`
behavior). Effective phase = service phase AND the meter/resource-type's
own inheriting-or-declared phase. For this scope, phase is
service-level only, so "skip on Draft" keys off
`ServiceConfiguration.spec.phase`.

## Files to delete

### Types
- `api/v1alpha1/meterdefinition_types.go`
- `api/v1alpha1/monitoredresourcetype_types.go`

### Controllers
- `internal/controller/meterdefinition_controller.go`
- `internal/controller/monitoredresourcetype_controller.go`
- `internal/controller/indexers.go` (rewrite; cross-resource indexing no longer needed)

### Webhooks
- `internal/webhook/v1alpha1/meterdefinition_webhook.go`
- `internal/webhook/v1alpha1/monitoredresourcetype_webhook.go`

### Validation
- `internal/validation/meterdefinition.go` + `_test.go`
- `internal/validation/monitoredresourcetype.go` + `_test.go`

### Manifests / samples / e2e
- `config/base/crd/bases/services.miloapis.com_meterdefinitions.yaml`
- `config/base/crd/bases/services.miloapis.com_monitoredresourcetypes.yaml`
- `config/components/iam/protected-resources/meterdefinition.yaml`
- `config/components/iam/protected-resources/monitoredresourcetype.yaml`
- `config/samples/services_v1alpha1_meterdefinition.yaml`
- `config/samples/services_v1alpha1_monitoredresourcetype.yaml`
- `test/e2e/meter-definition-lifecycle/`
- `test/e2e/monitored-resource-type-lifecycle/`

## Files to keep

- `api/v1alpha1/service_types.go` (Service is still an identity record)
- `api/v1alpha1/common_types.go` (`Phase`, `CatalogStatus`, `ProducerProjectReference`)
- `api/v1alpha1/groupversion_info.go`
- `internal/controller/service_controller.go`
- `internal/webhook/v1alpha1/service_webhook.go`
- `internal/validation/service.go` + `phase.go`
- Service-related manifests, samples, e2e
- `internal/controller/billingmeterdefinition_controller.go` +
  `billingmonitoredresourcetype_controller.go` — **refactor**, don't
  delete: merge into a single `ServiceConfiguration`-driven fan-out that
  iterates over `spec.meters` and `spec.monitoredResourceTypes` and SSAs
  the corresponding billing objects. Per-meter/per-type translation
  logic is preserved verbatim.

## New code to write

1. **`api/v1alpha1/serviceconfiguration_types.go`** —
   `ServiceConfiguration`, `ServiceConfigurationSpec`, and the value
   types `Meter`, `MonitoredResourceType`, `MeterMeasurement`,
   `MeterBilling`, `MonitoredResourceLabel`, `GVKRef`. Cluster-scoped.
   `metadata.name` = service's reverse-DNS slug.
2. **`internal/controller/serviceconfiguration_controller.go`** — main
   reconciler. Per reconcile: validate, then call the billing fan-out
   (the only consumer for now).
3. **Billing fan-out (refactored from existing controllers)** —
   `internal/controller/billing_fanout.go`. Builds the desired set of
   billing `MeterDefinition` and `MonitoredResourceType` objects from
   `ServiceConfiguration.spec`, SSAs them, and deletes previously-managed
   objects no longer in the desired set.
4. **`internal/webhook/v1alpha1/serviceconfiguration_webhook.go`** —
   validating webhook for intra-document consistency:
   - `meter.name` uniqueness within `spec.meters`
   - `monitoredResourceType.type` uniqueness within
     `spec.monitoredResourceTypes`
   - `meter.monitoredResourceTypes[]` non-empty and every entry
     resolves to an element of `spec.monitoredResourceTypes[].type`
   - Name-prefix rules: each meter and each resource type prefixed by
     the referenced service's canonical name
   - Immutability: `meter.name`, `meter.measurement.aggregation`,
     `meter.measurement.unit`, `monitoredResourceType.type`, and
     `monitoredResourceType.gvk` immutable once the `ServiceConfiguration`
     is `Published` (webhook diffs against the previous stored object)
5. **`internal/validation/serviceconfiguration.go`** — pure validation
   logic called by the webhook (follows the existing `validation/*.go`
   pattern).
6. **Manifests**: new `ServiceConfiguration` CRD, webhook config, RBAC
   kept to the two billing groups the fan-out touches.

## Billing repo changes (separate PR, prerequisite)

- Add `spec.monitoredResourceTypes: []string` (required, min 1) to
  `billing.miloapis.com/MeterDefinition`. Semantics follow Google's
  `MetricDescriptor.monitored_resource_types`: each entry names a
  `billing.miloapis.com/MonitoredResourceType` that emits events for
  this meter. Additive change; safe for existing data once backfilled.
- Otherwise billing's CRDs stay as-is. They are now understood as
  implementation details of billing, programmed by the services
  operator and not authored by providers directly.

## Open questions to settle before coding

1. **Per-item immutability** — for MVP is service-level phase
   sufficient, or do we need per-meter phase to allow one meter to be
   `Deprecated` while the service as a whole stays `Published`?
   Answered earlier as "not for now" → service-level only. Calling it
   out here so it's not re-litigated.
2. **Status surface on `ServiceConfiguration`** — one top-level `Ready`
   + one `BillingFanOutHealthy` condition (compact) vs. per-meter /
   per-resource-type conditions (busy). Lean: compact. Per-item status
   lives on the downstream billing objects themselves.
3. **CRD cleanup during install** — deleting the old
   `MeterDefinition` / `MonitoredResourceType` CRDs drops any existing
   instances. Is there data in dev/staging clusters we need to migrate,
   or can we rely on `ServiceConfiguration` authoring from scratch?

## Sequencing

1. Billing repo: add `spec.monitoredResourceTypes: []string` to
   `billing.miloapis.com/MeterDefinition`, release a tagged version of
   `go.miloapis.com/billing/api/v1alpha1`. Prerequisite for step 3.
2. Land the new types + deepcopy + `ServiceConfiguration` CRD manifest
   (no controller yet).
3. Land the webhook and validation.
4. Refactor the two billing fan-out controllers into a single
   `ServiceConfiguration`-driven reconciler.
5. Write the main `ServiceConfiguration` controller (status + drives
   fan-out).
6. Delete the old types, controllers, webhooks, manifests, samples,
   e2e.
7. New e2e: one `ServiceConfiguration` happy-path that asserts the
   expected billing `MeterDefinition` + `MonitoredResourceType` objects
   materialize with correct fields, labels, and owner refs; plus a
   validation-error case (name-prefix mismatch).
