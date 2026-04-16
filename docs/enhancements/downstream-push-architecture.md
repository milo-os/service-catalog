# Enhancement: Downstream Push Architecture

**Status:** Approved for implementation
**Scope:** Services operator pushes `MeterDefinition` and `MonitoredResourceType` into
billing. Billing and quota remain independently deployable. Providers (e.g.
amberflo-provider) integrate with billing/quota types only — not services types.

---

## Problem

The current architecture has `amberflo-provider` importing `go.miloapis.com/services`
at compile time to watch `services.MeterDefinition`. This creates a hard dependency:

- Providers cannot operate without the services control plane running
- Any system that wants to integrate with meters must understand services types
- Billing and quota cannot be deployed standalone — they have no independent meter
  or resource type representations of their own

## Goal

- **Billing and quota are independently deployable.** They define their own CRD types
  and can be managed directly without the services operator.
- **Services is the governance source of truth.** When the services operator is running,
  it owns and pushes meter and resource type definitions into downstream systems.
- **Providers integrate with billing/quota only.** `amberflo-provider` and future
  providers watch billing types, not services types.
- **Ownership is signalled via labels**, not type coupling:
  ```
  services.miloapis.com/service: <spec.owner.service>
  app.kubernetes.io/managed-by: services-operator
  ```

## Architecture After This Change

```
User creates services.MeterDefinition
         │
         ▼  services operator reconciles
billing.MeterDefinition  (billing's own type, owned by services operator)
         │
         ▼  amberflo-provider reconciles billing's type
    Amberflo API
```

Dependency graph:

```
services ──imports──► billing   (new)

amberflo-provider ──imports──► billing   (already exists)
amberflo-provider  ✗  services           (removed in Phase 4)

billing   ✗  services
quota     ✗  services
quota     ✗  billing
```

No circular dependencies. Services is the only repo that gains a new import.

## Owner References and GC

Services operator sets an owner reference on every billing object it creates,
pointing back to the originating services object. Both types are cluster-scoped,
so cross-namespace owner reference restrictions do not apply. When a
`services.MeterDefinition` is deleted, Kubernetes GC cascades the delete to
the owned `billing.MeterDefinition` automatically. No finalizer is needed in
the services operator for this purpose.

## RBAC Model

Two deployment modes, expressed as Kustomize overlays in the billing repo:

- **standalone** — billing operator service account has full `create/update/delete`
  on `meterdefinitions` and `monitoredresourcetypes`. Used when billing is deployed
  without the services operator.
- **managed** — billing operator service account has `get/list/watch/update/patch`
  only (for status writes). `create/delete` is restricted to the services operator
  service account. Used when the services operator is present.

A validating webhook on both billing types enforces this at admission time: it rejects
any `create/update/delete` where the object carries
`app.kubernetes.io/managed-by: services-operator` but the requesting service account
is not the services operator.

---

## Implementation Plan

### Phase 1 — Add MeterDefinition and MonitoredResourceType to Billing

**Repo:** `go.miloapis.com/billing`

#### 1.1 Add `MeterDefinition` type

File: `api/v1alpha1/meterdefinition_types.go`

Fields:
- `spec.meterName` — string, immutable, reverse-DNS format (e.g. `compute.miloapis.com/cpu-seconds`)
- `spec.phase` — `Draft | Published | Deprecated | Retired`
- `spec.displayName` — string, mutable
- `spec.description` — string, mutable
- `spec.measurement.aggregation` — enum: `Sum | Max | Min | Count | UniqueCount | Latest | Average`, immutable
- `spec.measurement.unit` — UCUM string, immutable
- `spec.measurement.dimensions[]` — list of strings, additive only
- `spec.billing.consumedUnit` — UCUM string
- `spec.billing.pricingUnit` — UCUM string

Status (embed existing `CatalogStatus` pattern from services if extractable, otherwise inline):
- `conditions[]`
- `observedGeneration`
- `publishedAt`

No `spec.owner` field. Ownership is expressed exclusively via labels:
```
services.miloapis.com/service: <service-name>
app.kubernetes.io/managed-by: services-operator
```

#### 1.2 Add `MonitoredResourceType` type

File: `api/v1alpha1/monitoredresourcetype_types.go`

Fields:
- `spec.resourceTypeName` — string, immutable
- `spec.phase` — `Draft | Published | Deprecated | Retired`
- `spec.displayName` — string, mutable
- `spec.description` — string, mutable
- `spec.gvk.group` — string, immutable
- `spec.gvk.kind` — string, immutable
- `spec.labels[]` — list of `{name, required, description, allowedValues[]}`, additive only

Status: same pattern as `MeterDefinition` above.

No `spec.owner` field. Ownership via labels same as above.

#### 1.3 Add validating webhooks for both types

- `meterName`, `measurement.aggregation`, `measurement.unit` — immutable after create
- `resourceTypeName`, `gvk.group`, `gvk.kind` — immutable after create
- `spec.measurement.dimensions` and `spec.labels` — additive only (cannot remove entries)
- Phase transition validation (same rules as services)

#### 1.4 Add controllers for both types

- Reconcile `status.conditions` and `status.observedGeneration`
- Set `Ready` condition based on phase and field validity
- No object creation logic — controllers only react to what is written

#### 1.5 Generate manifests

```bash
task generate
task manifests
```

---

### Phase 2 — Services Operator Pushes to Billing

**Repo:** `go.miloapis.com/services`

#### 2.1 Add billing dependency

```bash
go get go.miloapis.com/billing
```

Add `replace go.miloapis.com/billing => ../billing` to `go.mod` for local development.

#### 2.2 Add `BillingMeterDefinitionReconciler`

File: `internal/controller/billingmeterdefinition_controller.go`

- Watches: `services.miloapis.com/v1alpha1/MeterDefinition`
- On reconcile:
  1. Skip if `spec.phase` is `Draft` (only push `Published`, `Deprecated`, `Retired`)
  2. Build a `billing.MeterDefinition` with all spec fields mapped 1:1 (see field
     mapping table below)
  3. Set labels:
     ```
     services.miloapis.com/service: <spec.owner.service>
     app.kubernetes.io/managed-by: services-operator
     ```
  4. Set owner reference pointing to the `services.MeterDefinition`
  5. Server-side apply using field manager `services-operator` — preserves any
     billing-specific fields set by the billing operator
- On delete: owner reference cascades via Kubernetes GC, no explicit cleanup needed

RBAC markers required on this controller:
```go
//+kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions,verbs=get;list;watch;create;update;patch;delete
```

#### 2.3 Add `BillingMonitoredResourceTypeReconciler`

File: `internal/controller/billingmonitoredresourcetype_controller.go`

Same pattern as above for `MonitoredResourceType`. Field mapping:
- `spec.resourceTypeName`, `spec.phase`, `spec.displayName`, `spec.description`
- `spec.gvk.group`, `spec.gvk.kind`
- `spec.labels[]`

RBAC markers:
```go
//+kubebuilder:rbac:groups=billing.miloapis.com,resources=monitoredresourcetypes,verbs=get;list;watch;create;update;patch;delete
```

#### 2.4 Update `cmd/services/main.go`

- Register `billingv1alpha1` scheme: `billingv1alpha1.AddToScheme(scheme)`
- Instantiate and start both new controllers

#### 2.5 Generate manifests

```bash
task generate
task manifests
```

#### Field Mapping: services → billing

| services field | billing field | notes |
|---|---|---|
| `spec.meterName` | `spec.meterName` | immutable |
| `spec.phase` | `spec.phase` | all four states |
| `spec.displayName` | `spec.displayName` | mutable |
| `spec.description` | `spec.description` | mutable |
| `spec.measurement.aggregation` | `spec.measurement.aggregation` | immutable |
| `spec.measurement.unit` | `spec.measurement.unit` | immutable |
| `spec.measurement.dimensions` | `spec.measurement.dimensions` | additive |
| `spec.billing.consumedUnit` | `spec.billing.consumedUnit` | |
| `spec.billing.pricingUnit` | `spec.billing.pricingUnit` | |
| `spec.owner.service` | label `services.miloapis.com/service` | |
| (new) | label `app.kubernetes.io/managed-by: services-operator` | |

| services field | billing field | notes |
|---|---|---|
| `spec.resourceTypeName` | `spec.resourceTypeName` | immutable |
| `spec.phase` | `spec.phase` | all four states |
| `spec.displayName` | `spec.displayName` | mutable |
| `spec.description` | `spec.description` | mutable |
| `spec.gvk.group` | `spec.gvk.group` | immutable |
| `spec.gvk.kind` | `spec.gvk.kind` | immutable |
| `spec.labels[]` | `spec.labels[]` | additive |
| `spec.owner.service` | label `services.miloapis.com/service` | |
| (new) | label `app.kubernetes.io/managed-by: services-operator` | |

---

### Phase 3 — RBAC Lockdown in Billing

**Repo:** `go.miloapis.com/billing`

#### 3.1 Add Kustomize RBAC overlays

`config/rbac/overlays/standalone/` — grants billing operator service account:
```yaml
- apiGroups: ["billing.miloapis.com"]
  resources: ["meterdefinitions", "monitoredresourcetypes"]
  verbs: ["create", "get", "list", "watch", "update", "patch", "delete"]
```

`config/rbac/overlays/managed/` — grants billing operator service account:
```yaml
- apiGroups: ["billing.miloapis.com"]
  resources: ["meterdefinitions", "monitoredresourcetypes"]
  verbs: ["get", "list", "watch", "update", "patch"]
```

And grants services operator service account in the managed overlay:
```yaml
- apiGroups: ["billing.miloapis.com"]
  resources: ["meterdefinitions", "monitoredresourcetypes"]
  verbs: ["create", "get", "list", "watch", "update", "patch", "delete"]
```

#### 3.2 Add ownership-enforcement webhook

Add a validating webhook on `billing.MeterDefinition` and `billing.MonitoredResourceType`
that rejects `create/update/delete` requests where:
- The object has label `app.kubernetes.io/managed-by: services-operator`
- AND the request `UserInfo` is not the services operator service account

This enforces ownership at admission time independently of role binding configuration.

---

### Phase 4 — Migrate Amberflo-Provider off Services Types

**Repo:** `go.datum.net/amberflo-provider`

#### 4.1 Update `MeterDefinitionReconciler`

File: `internal/controller/meterdefinition_controller.go` (or equivalent)

- Change the watched type from `servicesv1alpha1.MeterDefinition` to
  `billingv1alpha1.MeterDefinition`
- Field names are identical — aggregation mapping logic (`Sum → sum_of_all_usage`,
  `UniqueCount → active_users`, etc.) is unchanged
- Move the finalizer (`amberflo.miloapis.com/meter`) to operate on the billing type

#### 4.2 Update `cmd/amberflo-provider/main.go`

- Remove: `servicesv1alpha1.AddToScheme(scheme)`
- Keep: `billingv1alpha1.AddToScheme(scheme)`

#### 4.3 Remove services dependency

In `go.mod`:
- Remove `go.miloapis.com/services` require directive
- Remove `replace go.miloapis.com/services => ../services` directive

```bash
go mod tidy
```

---

## What Does Not Change

- Services' existing controllers, webhooks, and governance lifecycle
- Billing's `BillingAccount` and `BillingAccountBinding` types and controllers
- Amberflo-provider's `BillingAccountReconciler` (already watches billing types)
- Quota — already independent, no integration changes in this enhancement
