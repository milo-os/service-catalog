---
id: locations-platform-primitive
title: Locations as Platform Primitives for Service Consumers
status: draft
created: 2026-05-26
updated: 2026-08-25
author: Scot Wells
---

# Locations as Platform Primitives for Service Consumers

> **Superseded in part.** `Location` now lives in
> [`milo-os/locations`](https://github.com/milo-os/locations) as
> `locations.miloapis.com/v1alpha1`, alongside `LocationClass` and
> `ServingLocation`. Two things below no longer hold:
>
> - **There is no `LocationBinding` in the new API.** The consumer-facing object
>   is a `Location` projected into the consumer's control plane — same kind as
>   the platform declares, so consumers read one type wherever they look. This
>   catalog projects both objects for now and stops writing `LocationBinding`
>   once its remaining readers move; see the migration note below.
> - **A location's class is an open reference, not a closed enum.**
>   `spec.locationClassRef` names a `LocationClass` object, and that class,
>   rather than a `provider` block on the Location, carries the provider
>   configuration. `ServiceConfiguration.spec.locations.supportedClasses`
>   accordingly holds class names rather than a fixed set of values.
>
> The three-gate model, the ownership split, and the rest of the reasoning below
> are unchanged.

## Choosing where locations are read from

`ServicesOperator.locationSource` names the group locations are read from, and
it is the only group read. It defaults to `networking.datumapis.com/v1alpha`,
which is what control planes serve today; setting it to
`locations.miloapis.com/v1alpha1` moves a deployment onto the locations service.

There is deliberately no automatic fallback between the two. During the cutover
both groups can be installed at once, which is exactly when an implicit
"new first, old second" ordering would start deciding on its own — and if only
some locations had migrated it would resolve some from one group and some from
the other, with nobody having chosen that. Making it configuration also means
rollback is a config change rather than an undeploy, and that which source a
deployment is on can be read off rather than inferred from which CRDs happen to
be installed.

A source the control plane does not serve is a misconfiguration, not a missing
location. Every affected `ServiceAvailability` reports `Available=Unknown` with
reason `LocationSourceUnavailable` and a message naming the group, and re-checks
so it recovers on its own once the group is installed. Projection stops before
its prune step in that state, so an unreachable source cannot be mistaken for
"this project has no locations" and tear down projections that already exist.

Which group is *read* is independent of which kinds are *written*: a deployment
can serve consumers the new `Location` while still sourcing locations from the
old group.

## Migration off `LocationBinding`

`LocationBinding` cannot be dropped by this repo alone. Four readers outside it
still depend on the kind:

| Repo | Reader | Effect of removing the kind |
|---|---|---|
| `datum-cloud/network-services-operator` | `internal/controller/networkpresence_controller.go` | A presence with no binding for its location is refused |
| `datum-cloud/network-services-operator` | `test/e2e/network-presence-ready/chainsaw-test.yaml` | Applies a `LocationBinding` directly; the suite fails |
| `datum-cloud/compute` | `internal/webhook/v1alpha/workload_webhook.go` | Admission webhook: workload creation is **rejected**, not degraded |
| `datum-cloud/compute` | `internal/controller/workload_controller.go` | Workload reconciliation cannot resolve city codes |

The compute webhook is the sharp one: it fails closed, so removing the kind
rejects workload creation outright rather than degrading quietly.

So both objects are written to entitled projects, carrying the same gate verdict
in the same `Available` condition. Consumers move to `Location` at their own
pace. `LocationBinding` is removed here only once no reader is left, in a
separate change.

## Overview

This document answers a specific design question: after a consumer enables the compute
service via a `ServiceEntitlement`, how do they discover which locations are available to
them, and how does the system validate and enforce location constraints at API time and at
quota time?

The `Location` type currently lives in `network-services-operator` with an explicit TODO
to move it out. This design specifies where it moves, what triggers location visibility
in a project, and how location-scoped quota dimensions bind to that resource model. It
also covers how consumer-owned or provider-dedicated locations fit into the same resource
type without requiring a separate discovery surface.

---

## Requirements

### Functional Requirements

- FR1: Platform operators must be able to register `Location` resources representing PoPs
  managed by Datum or self-managed by customers.
- FR2: When a consumer enables the compute service (`ServiceEntitlement` for
  `compute.datumapis.com` becomes `Active`), the set of locations available to that
  project must be discoverable through the API.
- FR3: A consumer listing locations must see only locations their project is entitled to
  use, not all platform locations.
- FR4: `Workload.spec.placements[].cityCodes` validation must check only city codes
  reachable by the requesting project.
- FR5: The `compute.datumapis.com/location` dimension on quota `ResourceClaim` objects
  must refer to a stable location name that is consistent across the platform.
- FR6: When a `ServiceEntitlement` is deleted or suspended, project-level location
  visibility must be removed cleanly.
- FR7: The platform must support consumers having their own locations or dedicated
  infrastructure brought to them by a service provider, discoverable through the same
  API surface as platform-managed locations.
- FR8: `ServiceConfiguration` must declare which location classes the service supports,
  so that new PoPs of a supported class become automatically available to entitled projects
  without requiring a new configuration version.

### Non-Functional Requirements

- NFR1: Location discovery (`LIST`) must serve from cache; no platform cluster
  read-through on each consumer API call.
- NFR2: The resource model must not require per-project storage of full location specs —
  if a platform location changes its provider config, that change must not require
  updating thousands of per-project copies.
- NFR3: Consumer-owned and platform-managed locations must be discoverable through the
  same resource type and API endpoint. Two separate discovery surfaces (as in Azure's
  `customLocations` vs Regions model) must be avoided.
- NFR4: The type change from `networking.datumapis.com` to the target group must be
  achievable without a flag-day migration of all consumers simultaneously.

---

## Design

### Recommendation: Single `Location` Type, Differentiated by `locationClassName`

The `Location` type should **stay in the `networking.datumapis.com` API group** for now
but move its Go definition out of the NSO repository into a shared library importable by
`compute`, `infra-provider-gcp`, and NSO alike. The longer-term target group is
`resourcemanager.miloapis.com` — `Location` is a resource management primitive, not a
networking primitive, and the current home in NSO is explicitly marked with a TODO to
move it out. That rename requires milo-level work and is captured in a separate tracking
issue; it is orthogonal to solving the consumer-visibility problem now.

The market research finding that most directly shapes this recommendation: **Azure's
decision to create a separate `Microsoft.ExtendedLocation/customLocations` resource type
for consumer-owned locations is a known developer experience failure.** Consumers must
handle two different resource types, use a different `extendedLocation` field when
creating resources, and navigate two different discovery APIs. AWS's `ZoneType` field on a
shared `DescribeAvailabilityZones` surface is significantly more ergonomic.

Datum's `locationClassName` field (already present in the existing type) is the correct
mechanism for differentiation. All location variants — platform-managed PoPs,
provider-dedicated infrastructure, and consumer-owned sites — use the same `Location`
resource type and the same `LocationBinding` discovery surface. The class field drives
different controller behavior and IAM constraints, not a different resource type.

**Immediate action: extract to shared module, define `locationClassName` values as a
well-known set. Defer group rename to a separate RFC.**

### `locationClassName` Values

The following classes are defined. They are a typed enum in the Go type definition, not
free-form strings, to allow service providers to reliably reference them in
`ServiceConfiguration`.

| `locationClassName` | Operator | Example |
|---------------------|----------|---------|
| `datum-managed` | Datum platform team | Shared PoP in Chicago |
| `provider-dedicated` | Service provider, dedicated to specific consumer | Private rack at a consumer's facility, brought by Datum |
| `self-managed` | Consumer | Consumer-registered on-premises site |

This set is extensible. New classes require a platform-level decision (adding to the
enum), not a code change in every consuming operator.

### Three-Gate Model for Location Availability

A `LocationBinding` is only created — and remains `Available=True` — when all three gates
are open simultaneously. If any gate closes, the `Available` condition on the corresponding
`LocationBinding` is set to `False`; the binding is not deleted, which preserves quota
accounting continuity and avoids reconciler storms when a gate briefly toggles.

| Gate | Resource | Owner |
|------|----------|-------|
| 1. Service class support | `ServiceConfiguration.spec.locations.supportedClasses` | Service provider (immutable per config version) |
| 2. Infrastructure ready | `Location.status.conditions[Ready]` | Platform operator / infra controller |
| 3. Service operational | `ServiceAvailability.status.conditions[Available]` | Service operator per deployment |

`ServiceAvailability` (`catalog.miloapis.com/v1alpha1`) is a new resource that decouples
"the PoP exists and hardware is ready" from "this specific service is deployed and
validated at this PoP." Any service with a locality component creates a
`ServiceAvailability` object when it completes deployment and passes health checks at a
location. See [milo-os/service-catalog#24](https://github.com/milo-os/service-catalog/issues/24)
for the full design.

```yaml
apiVersion: catalog.miloapis.com/v1alpha1
kind: ServiceAvailability
metadata:
  name: compute.datumapis.com--us-central1-a
  namespace: services
spec:
  serviceRef:
    name: compute.datumapis.com
  locationRef:
    name: us-central1-a
    namespace: platform
status:
  conditions:
    - type: Available
      status: "True"
      reason: ServiceOperational
```

### Two-Tier Discovery Model

Market research confirms that no major platform exposes a single flat list of all
locations to all consumers without context. The right model has two tiers.

**Tier 1 — Service-level declaration**: The service provider declares, in
`ServiceConfiguration.spec.locations`, which location classes their service version
supports. This is the Datum equivalent of how GCP services implement their own
`projects.locations.list` — the service says "I run on PoPs of class `datum-managed` and
`provider-dedicated`." New PoPs of a supported class automatically become available to
all entitled projects when they come online; the service configuration version does not
need to change.

**Tier 2 — Project-level projection**: After a `ServiceEntitlement` activates, a
controller creates a `LocationBinding` per eligible `Location` in the project's namespace,
but only for locations where all three gates are open. This is the authoritative "what
locations does this project have access to" list. It covers both platform-managed
locations (projected by the entitlement reconciler) and consumer-owned or dedicated
locations (projected when the dedicated infrastructure is provisioned for that project).

The `LocationBinding` is the consumer-facing API. All three gates feed into it through
different controllers and different triggers, but the consumer always queries the same
resource type.

### `ServiceConfiguration.spec.locations`

Add a `locations` section to `ServiceConfiguration` that declares which location classes
the service version supports. The declaration uses class selectors, not specific location
names, so it does not need to be updated when new PoPs come online.

```yaml
apiVersion: catalog.miloapis.com/v1alpha1
kind: ServiceConfiguration
metadata:
  name: compute-v3-abc123
  namespace: services
spec:
  serviceRef:
    name: compute.datumapis.com
  version: "3.0.0"

  # Declare which location classes this service version supports.
  # The entitlement reconciler uses this to filter which platform
  # Location objects to project into entitled projects.
  locations:
    supportedClasses:
      - datum-managed
      - provider-dedicated
    # Optional: restrict to specific topology regions if the service
    # is not globally deployed in this configuration version.
    # topologySelectors:
    #   - topology.datum.net/region: us-central1

  # ... quota, billing, authorization sections unchanged ...
```

The `LocationBindingReconciler` (owned by service catalog, see Controller Ownership) reads
the active `ServiceConfiguration` for each entitlement and uses
`spec.locations.supportedClasses` together with the three-gate check (class support +
`Location.Ready` + `ServiceAvailability.Available`) to decide which locations to project.
`self-managed` locations are not automatically projected by the entitlement reconciler —
they are projected when the consumer registers their infrastructure (see the
Consumer-Owned and Dedicated Locations section).

### Resource Model

#### Platform-Level `Location` (shared type, new Go module home)

Locations in the platform control plane are managed by platform operators. The type is
unchanged in structure; what changes is the Go module where it is defined.

```yaml
apiVersion: networking.datumapis.com/v1alpha
kind: Location
metadata:
  name: us-central1-a
  namespace: platform
  labels:
    networking.datumapis.com/class: datum-managed
spec:
  locationClassName: datum-managed
  topology:
    topology.datum.net/city-code: ORD
    topology.datum.net/region: us-central1
  provider:
    gcp:
      projectId: datum-infra-prod
      region: us-central1
      zone: us-central1-a
status:
  conditions:
    - type: Ready
      status: "True"
      reason: LocationReady
```

#### Project-Level `LocationBinding` (new resource)

`LocationBinding` is a thin projection. It carries only the topology fields a consumer
needs for discovery and webhook validation, plus a reference to the canonical `Location`.
It intentionally does not copy provider-internal fields (GCP project ID, zone) that
consumers should not see and that change infrequently in ways that do not affect
consumers.

```yaml
apiVersion: networking.datumapis.com/v1alpha
kind: LocationBinding
metadata:
  name: us-central1-a
  namespace: default
  labels:
    networking.datumapis.com/location: us-central1-a
    networking.datumapis.com/class: datum-managed
    catalog.miloapis.com/service: compute.datumapis.com
spec:
  locationRef:
    name: us-central1-a
    namespace: platform
  # locationClassName copied for fast filtering without a platform lookup
  locationClassName: datum-managed
  # topology keys copied for city-code resolution at webhook admission time
  topology:
    topology.datum.net/city-code: ORD
    topology.datum.net/region: us-central1
status:
  conditions:
    - type: Available
      status: "True"
      reason: AllGatesOpen
      # Set False when any of the three gates closes:
      # - ServiceConfiguration.spec.locations.supportedClasses no longer includes the class
      # - Location.status.conditions[Ready] = False
      # - ServiceAvailability.status.conditions[Available] = False
```

The `networking.datumapis.com/class` label mirrors `spec.locationClassName` to allow
efficient label-selector queries without field selectors, which may not be indexed in all
project control plane configurations.

### Why `LocationBinding` and Not Direct `Location` Copies or Pure IAM Gating

Three options were evaluated:

**Option A: Copy full `Location` objects into the project namespace.** Creates full
replicas per project per location. At 100 locations and 10,000 projects that is 1,000,000
objects. When a Location's provider config changes, all copies need updating. When a new
PoP comes online, it must be added to every active project's namespace. Rejected.

**Option B: IAM gate on global `Location` list — no per-project objects.** Use an IAM
check in the webhook rather than per-project objects. GCP's `projects.locations.list`
works like this — the project is an authentication context, not a filter. However, this
requires a live IAM call on every Workload admission and does not give consumers a
Kubernetes-native discovery mechanism. The webhook's existing TODO notes exactly this
complexity. For consumer-owned and dedicated locations that are genuinely project-specific
(not globally available), pure IAM gating also breaks down because there is no global
list to filter. Rejected as the sole mechanism.

**Option C: `LocationBinding` as a thin projection (recommended).** Carries only topology
keys, the location class, and a reference to the canonical `Location`. Consumers get
`kubectl get locationbindings` in their project. Webhooks query from cache. The binding
controller keeps the `Available` condition current by watching upstream `Location`
readiness. Consumer-owned and dedicated locations use the same binding resource and the
same discovery surface. Accepted.

### Who Controls Location Access

**Platform operators** control which `datum-managed` `Location` objects exist and are
`Ready`. A location that is not `Ready` is never projected.

**Service configuration** controls which location classes are eligible for projection.
Only classes listed in `ServiceConfiguration.spec.locations.supportedClasses` are
projected when an entitlement activates.

**Dedicated infrastructure provisioning** controls which `provider-dedicated` locations
appear in a specific project. The service provider creates the `Location` object (with
`spec.locationClassName: provider-dedicated` and `spec.ownerProjectRef` pointing to the
consumer's project) and the entitlement reconciler or a dedicated infrastructure
controller projects it into that project's namespace.

**Consumer registration** controls which `self-managed` locations appear in the
consumer's project. The consumer (or an automated workflow) creates the `Location` object
with `spec.locationClassName: self-managed`; the appropriate controller projects it into
their project namespace.

---

## Consumer-Owned and Dedicated Locations

### The Problem This Solves

A consumer might contract for a dedicated rack at a colocation facility, or a service
provider might deploy edge infrastructure inside a consumer's data center. This dedicated
infrastructure should appear to the consumer as a location they can deploy workloads to —
using the exact same API surface as shared platform PoPs. It should appear in
`datumctl compute locations list`, it should be a valid city code in
`WorkloadPlacement.CityCodes`, and it should be a valid quota dimension value.

Azure failed at this by creating `Microsoft.ExtendedLocation/customLocations` as a
separate resource type requiring a different `extendedLocation` field in resource
creation. Datum avoids this by using the same `Location` type with a different
`locationClassName`.

### `Location` Schema Additions for Non-Platform Classes

The existing `Location` type gains one new optional field: `ownerProjectRef`. This field
is only set for `provider-dedicated` and `self-managed` locations and declares which
consumer project this location belongs to. It is the mechanism that allows the binding
controller to know which project's namespace to project the location into without having
to enumerate all projects.

```yaml
apiVersion: networking.datumapis.com/v1alpha
kind: Location
metadata:
  name: acme-corp-dfw-01
  namespace: platform
  labels:
    networking.datumapis.com/class: provider-dedicated
spec:
  locationClassName: provider-dedicated

  # ownerProjectRef is set for provider-dedicated and self-managed locations.
  # It declares which consumer project this location is dedicated to.
  # The binding controller uses this to project the location into the
  # correct project namespace rather than all entitled projects.
  ownerProjectRef:
    name: acme-corp-project   # Milo project name

  topology:
    topology.datum.net/city-code: DFW
    topology.datum.net/region: us-south1
    # Additional topology metadata for dedicated locations
    topology.datum.net/facility: equinix-da11
  provider:
    gcp:
      projectId: datum-customer-infra
      region: us-south1
      zone: us-south1-a
status:
  conditions:
    - type: Ready
      status: "True"
      reason: LocationReady
```

For a `self-managed` location (consumer-registered infrastructure not on GCP):

```yaml
apiVersion: networking.datumapis.com/v1alpha
kind: Location
metadata:
  name: acme-corp-on-prem-chicago-01
  namespace: platform
  labels:
    networking.datumapis.com/class: self-managed
spec:
  locationClassName: self-managed
  ownerProjectRef:
    name: acme-corp-project
  topology:
    topology.datum.net/city-code: ORD
    topology.datum.net/facility: acme-chicago-dc1
  # provider is omitted or uses a future self-managed provider block
status:
  conditions:
    - type: Ready
      status: "True"
      reason: LocationReady
```

### `LocationBinding` for a Dedicated Location

The binding created in the consumer's project namespace for a dedicated location looks
identical in structure to a platform-managed binding. The `locationClassName` label is
the only difference visible to the consumer.

```yaml
apiVersion: networking.datumapis.com/v1alpha
kind: LocationBinding
metadata:
  name: acme-corp-dfw-01
  namespace: default
  labels:
    networking.datumapis.com/location: acme-corp-dfw-01
    networking.datumapis.com/class: provider-dedicated
    catalog.miloapis.com/service: compute.datumapis.com
spec:
  locationRef:
    name: acme-corp-dfw-01
    namespace: platform
  locationClassName: provider-dedicated
  topology:
    topology.datum.net/city-code: DFW
    topology.datum.net/region: us-south1
    topology.datum.net/facility: equinix-da11
status:
  conditions:
    - type: Available
      status: "True"
      reason: LocationReady
```

### Provisioning Triggers by Class

| `locationClassName` | Who creates the `Location` | Who creates the `LocationBinding` | Trigger |
|---------------------|---------------------------|-----------------------------------|---------|
| `datum-managed` | Platform operator | `ComputeServiceEntitlementReconciler` | `ServiceEntitlement` activates for `compute.datumapis.com` |
| `provider-dedicated` | Service provider (or automation) | `DedicatedLocationReconciler` (new) or the same entitlement reconciler using `ownerProjectRef` | Dedicated infrastructure is provisioned; `Location` object is created with `ownerProjectRef` set |
| `self-managed` | Consumer (or consumer automation) | `SelfManagedLocationReconciler` (new) or same mechanism | Consumer registers their infrastructure; `Location` object is created |

For Phase 1 implementation, `provider-dedicated` and `self-managed` bindings can be
created by the same `ComputeServiceEntitlementReconciler` using a secondary watch on
`Location` objects filtered by `ownerProjectRef`. The split into separate controller
types can happen when the volume or complexity warrants it.

### Referencing AWS Outposts as a Design Precedent

AWS Outposts require declaring a parent AZ (`AvailabilityZone` field) when creating an
Outpost, making it an extension of existing platform infrastructure. Datum's
`provider-dedicated` locations follow the same principle: the `topology` map should
include a `topology.datum.net/region` key referencing the nearest platform region, even
if the physical hardware is in a consumer facility. This anchors the dedicated location
to the platform topology graph and allows the scheduler to make proximity-aware decisions.

---

## Lifecycle: What Happens When a `ServiceEntitlement` Activates

The `LocationBindingReconciler` lives in the **service catalog operator** (not the compute
operator) because the three-gate logic is a generic platform concern: any service with a
locality component follows the same pattern. The compute operator participates only at the
quota and validation boundary.

**On activation** (`spec.serviceRef.name == "compute.datumapis.com"` and
`status.phase == Active`):

1. `LocationBindingReconciler` (service catalog) reads the active `ServiceConfiguration`
   referenced by the entitlement to obtain `spec.locations.supportedClasses`.
2. It lists all platform `Location` objects where:
   - `spec.locationClassName` ∈ `supportedClasses`
   - `status.conditions[Ready] = True`
   - A `ServiceAvailability` object exists for `(service=compute.datumapis.com, location=X)`
     with `status.conditions[Available] = True`
   - Either no `ownerProjectRef` (platform-wide) OR `ownerProjectRef.name` matches this project
3. For each qualifying `Location`, creates or updates a `LocationBinding` in the project's
   namespace in the project control plane with `Available=True`. Creation is idempotent.
4. Adds finalizer `catalog.miloapis.com/location-binding-controller` to the
   `ServiceEntitlement` to gate cleanup.

**On a new `datum-managed` `Location` becoming `Ready`** (a new PoP comes online):

The `LocationBindingReconciler` detects the `Ready=True` transition. It then checks
whether a `ServiceAvailability` object exists for the new location. If it does, bindings
are created across all project control planes with an active entitlement for the service.
If it does not, no binding is created until gate 3 opens (the service must be deployed and
validated at the PoP before consumers can target it).

**On a `ServiceAvailability` toggling to `Available=False`** (a PoP goes into maintenance
or a service degradation is declared):

The `LocationBindingReconciler` updates `Available=False` on the corresponding
`LocationBinding` objects in all entitled project namespaces. The binding is **not
deleted** — this preserves quota accounting continuity and avoids reconciler storms on
brief toggles. Webhooks treat `Available=False` bindings as invalid city-code candidates.

When the `ServiceAvailability` returns to `Available=True`, the reconciler sets
`Available=True` on all affected bindings.

**On a `ServiceAvailability` toggling to `Available=False`** for one service at a location
where multiple services are available: only `LocationBinding` objects carrying
`catalog.miloapis.com/service: <that-service>` are affected. Other services' bindings at
that location are untouched.

**On a `Location` transitioning to `Ready=False`** (hardware failure, maintenance):

The `LocationBindingReconciler` sets `Available=False` on all `LocationBinding` objects
for that location across all project control planes, regardless of service. No binding is
deleted.

**On a `provider-dedicated` or `self-managed` `Location` being created**:

The `LocationBindingReconciler` detects the new `Location` via a watch filtered to
non-`datum-managed` classes. It reads `spec.ownerProjectRef` to identify the target
project, then applies the same three-gate check. If all gates are open, the binding is
created in that project's namespace only.

**On entitlement deletion** (`.metadata.deletionTimestamp` set):

1. `LocationBindingReconciler` deletes all `LocationBinding` objects in the project's
   namespace carrying `catalog.miloapis.com/service: compute.datumapis.com`.
2. Once all bindings are gone, removes the finalizer from the `ServiceEntitlement`.

Deleting a `provider-dedicated` or `self-managed` `Location` object triggers the same
cleanup via the `LocationBindingReconciler` watch on that class.

### Controller Ownership

| Controller | Repository | Responsibility |
|------------|-----------|----------------|
| `LocationReconciler` | NSO or shared location operator | Manages platform `Location` objects; sets `Ready` condition (gate 2) |
| `ServiceAvailabilityReconciler` | `milo-os/service-catalog` | Created by each service operator when deployed and validated at a PoP; sets `Available` condition (gate 3). See milo-os/service-catalog#24. |
| `LocationBindingReconciler` | `milo-os/service-catalog` | Watches all three gates; creates and maintains `LocationBinding` projections in project namespaces; handles fan-out for new PoPs; handles entitlement deletion cleanup |
| `ComputeServiceEntitlementReconciler` | `datum-cloud/compute` | Compute-specific entitlement logic (quota allocation, RBAC provisioning); does NOT manage `LocationBinding` objects — that is the service catalog's responsibility |

---

## Consumer-Facing API

After entitlement activation a project admin sees all location classes in a single list:

```
$ datumctl compute locations list --project acme-corp-project
NAME                           CLASS               CITY   REGION       AVAILABLE
us-central1-a                  datum-managed       ORD    us-central1  True
eu-west1-b                     datum-managed       LHR    eu-west1     True
ap-northeast1-a                datum-managed       NRT    ap-northeast1 True
acme-corp-dfw-01               provider-dedicated  DFW    us-south1    True
acme-corp-on-prem-chicago-01   self-managed        ORD    —            True
```

The underlying resource is `LocationBinding` in the project's namespace. The CLI calls
`GET /apis/networking.datumapis.com/v1alpha/namespaces/{namespace}/locationbindings`. All
location classes appear in the same list. The `CLASS` column maps to
`spec.locationClassName`. Consumers do not interact with the platform-level `Location`
objects directly.

This matches GCP's `gcloud compute zones list` behavior — a single filtered list of
what the project can actually use — while extending it to include consumer-owned
infrastructure that GCP cannot model in its resource hierarchy at all.

The `WorkloadPlacement.CityCodes` field continues to use IATA codes (e.g., `ORD`). The
webhook resolves city codes by listing `LocationBinding` objects in the project's
namespace and extracting `spec.topology["topology.datum.net/city-code"]`. Consumer-owned
locations in Chicago appear with `city-code: ORD` alongside the platform PoP in Chicago;
if both are `Available=True`, the scheduler resolves which physical location to use based
on the full `LocationReference` in `Instance.status.location`, not the city code.

The webhook change that closes the TODO at `workload_webhook.go:100-103`:

```go
// Replaces the current cluster-wide Location list
var bindings networkingv1alpha.LocationBindingList
if err := clusterClient.List(ctx, &bindings, client.InNamespace(workload.Namespace)); err != nil {
    return nil, fmt.Errorf("failed to list location bindings: %w", err)
}

validCityCodes := sets.Set[string]{}
for _, binding := range bindings.Items {
    available := apimeta.FindStatusCondition(binding.Status.Conditions, "Available")
    if available != nil && available.Status == metav1.ConditionTrue {
        if cityCode, ok := binding.Spec.Topology["topology.datum.net/city-code"]; ok {
            validCityCodes.Insert(cityCode)
        }
    }
}
```

The project-scoped binding list is the per-caller access check by construction. A
project only sees bindings that were projected for it — platform locations via entitlement
activation, dedicated and self-managed locations via infrastructure provisioning or
consumer registration.

---

## Quota Integration

The `compute.datumapis.com/location` dimension on `ResourceClaim` objects uses the
`Location` name (e.g., `us-central1-a` or `acme-corp-dfw-01`) — not the city code. This
is already established in the `ServiceConfiguration` quota bucket definitions and works
uniformly across all location classes.

The `InstanceReconciler` populates the location dimension from
`Instance.status.location.name` (a `LocationReference`), which the scheduling system sets
when it assigns an instance to a specific `Location`.

```yaml
# ResourceClaim dimension labels (per-instance, platform-managed location)
spec:
  resources:
    - name: compute.datumapis.com/instances/cpu
      quantity: "4"
      dimensionLabels:
        resourcemanager.miloapis.com/project: acme-corp-project
        compute.datumapis.com/location: us-central1-a

# ResourceClaim dimension labels (per-instance, dedicated location)
spec:
  resources:
    - name: compute.datumapis.com/instances/cpu
      quantity: "4"
      dimensionLabels:
        resourcemanager.miloapis.com/project: acme-corp-project
        compute.datumapis.com/location: acme-corp-dfw-01
```

The quota system treats both claims identically — it evaluates numerical limits against
the dimension combination. Quota allowances can be scoped per-location if desired (a
dedicated location might have a higher or separate quota bucket than shared PoPs), but
the dimension model supports this without structural changes.

**The quota system does not need to know about `LocationBinding` existence.** Quota
evaluates limits against dimension values. Whether the location is valid for the project
is enforced earlier, at Workload admission, by the webhook checking `LocationBinding`
availability.

### Security Considerations

`LocationBinding` objects in a project namespace are readable by any project member with
`networking.datumapis.com/locationbindings.list` permission (granted via the compute
`ServiceConfiguration`'s IAM role definitions). They are writable only by the platform
service accounts running `ComputeServiceEntitlementReconciler` and
`LocationBindingReconciler`.

Platform `Location` objects remain in the platform control plane and are not directly
accessible by project users. The `ownerProjectRef` field on non-`datum-managed` locations
is readable only by platform operators and the binding controller service account.

Provider-internal fields on `Location` (GCP project ID, zone) are never copied into
`LocationBinding` and never exposed to consumers.

---

## Migration Path

| Phase | Current state | Target state | Action |
|-------|--------------|--------------|--------|
| 0 (now) | `Location` type in NSO; webhook lists all platform locations globally | Shared type module with `ownerProjectRef` field | Extract type to shared module; add `ownerProjectRef` field and `locationClassName` enum; no behavior change |
| 1 | Webhook uses cluster-wide `Location` list; no per-project visibility | `LocationBinding` per project; webhook uses project-scoped list; three-gate model enforced | Implement `ServiceAvailability` CRD + `LocationBindingReconciler` in `milo-os/service-catalog`; compute service operator creates `ServiceAvailability` on each PoP deployment; update webhook to use `LocationBinding` list; add `locations` section to compute `ServiceConfiguration` |
| 2 | No location dimension on `ResourceClaim`; no dedicated location support | Location dimension populated on all new claims; `provider-dedicated` bindings created on `Location` creation | Update `InstanceReconciler` claim construction; `LocationBindingReconciler` handles non-`datum-managed` classes via `ownerProjectRef` |
| 3 (future) | `self-managed` location registration is manual | Consumer-facing registration workflow | Consumer-facing API and automation for registering self-managed sites; may include a `LocationRegistration` request resource |
| 4 (future) | `networking.datumapis.com/Location` | `resourcemanager.miloapis.com/Location` | Separate RFC; requires milo-level work and graduated rename; tracked in milo-os tracking issue |

Phases 0 and 1 can proceed in parallel on the type extraction side; Phase 1 requires
`ServiceAvailability` to be implemented in service catalog before the
`LocationBindingReconciler` can apply gate 3. Phase 2 requires Phase 1 because the
webhook must enforce valid locations before claims can be trusted to carry a valid location
dimension. Phase 3 (self-managed registration UX) and Phase 4 (group rename) are
independent of each other.

---

## Handoff

### Decisions Made

- **Decision 1: Single `Location` type differentiated by `locationClassName`, not separate
  resource types.** Rationale: Azure's `Microsoft.ExtendedLocation/customLocations` is a
  documented developer experience failure — two resource types, two discovery surfaces, a
  different `extendedLocation` field in resource creation. AWS's `ZoneType` field on a
  shared API surface is the right model. Datum's existing `locationClassName` field is
  already the correct mechanism.

- **Decision 2: `LocationBinding` as thin projection, covering all location classes.**
  Rationale: avoids O(locations * projects) storage explosion; eliminates the update
  fan-out problem when platform locations change; provides a single discovery surface for
  platform-managed, dedicated, and self-managed locations without a global read.

- **Decision 3: `ServiceConfiguration.spec.locations` uses class selectors, not specific
  location names.** Rationale: if the configuration named specific locations, adding a new
  PoP would require a new configuration version and a re-activation rollout across all
  entitled projects. Class selectors allow the service to declare "I work on all
  `datum-managed` locations" and automatically benefit from new PoPs.

- **Decision 4: City-code API stays unchanged.** `WorkloadPlacement.CityCodes` remains
  the consumer-facing placement primitive. The webhook resolves codes through
  `LocationBinding.spec.topology` rather than `Location.spec.topology`. No consumer API
  change is needed.

- **Decision 5: Location name (not city code) is the quota dimension value.**
  `compute.datumapis.com/location` on `ResourceClaim` uses `Location.metadata.name`,
  which is globally unique. City codes map N:1 to locations (multiple PoPs per city, both
  platform and dedicated) so they are unsuitable as quota dimensions.

- **Decision 6: `ownerProjectRef` on `Location` for non-`datum-managed` classes.**
  Rationale: analogous to AWS Outposts always referencing a parent AZ. The field anchors
  dedicated and self-managed locations to the consumer project that owns them, allowing
  the binding controller to project them into the correct namespace without a global
  project enumeration.

- **Decision 7: `LocationBindingReconciler` lives in `milo-os/service-catalog`, not
  `datum-cloud/compute`.** The three-gate pattern (class support + Location ready +
  ServiceAvailability available) is generic — any service with a locality component uses
  the same logic. Placing it in the compute operator would require duplicating it for every
  other service (object-storage, cdn, etc.). Service catalog owns the binding lifecycle;
  compute participates only at the quota and admission webhook boundary.

- **Decision 8: `ServiceAvailability` in `catalog.miloapis.com` replaces per-service labels
  on `Location` objects.** Putting a `compute.datumapis.com/service-ready` label on a
  platform primitive `Location` creates a coupling violation: the platform primitive would
  accumulate one label per service, making it impossible to evolve the set of services
  independently. `ServiceAvailability` is a separate resource in the service catalog API
  group — each service operator creates it, the service catalog reconciler reads it as gate
  3. Tracked in milo-os/service-catalog#24.

- **Decision 9: API group target is `resourcemanager.miloapis.com`.** `Location` is a
  resource management primitive: it describes where platform resources exist and are
  accessible. `networking.datumapis.com` is incorrect for the same reason
  `Microsoft.ExtendedLocation` is confusing — it mixes infrastructure concerns with
  networking API group semantics. The rename requires a milo-level RFC and graduated
  migration; it is captured as a tracking item and does not block Phases 0-3.

### Open Questions

- **Q1: `ServiceConfiguration.spec.locations` topology selectors (non-blocking).** The
  design reserves a `topologySelectors` field in `ServiceConfiguration.spec.locations` for
  restricting the service to specific regions within a supported class (e.g., a
  configuration version that only covers `us-central1` initially). Should this be in scope
  for Phase 1 or deferred until there is a concrete use case? Proceed without topology
  selectors for Phase 1 — all `Ready` locations of a supported class are projected.

- **Q2: `provider-dedicated` location provisioning workflow (non-blocking).** The design
  says the service provider creates the `Location` object with `ownerProjectRef` set. What
  is the exact workflow? Does the service provider do this through a control plane API, a
  Helm chart, or a dedicated `LocationProvisioningRequest` resource? Phase 2 can proceed
  with direct `Location` creation by platform operators; a self-service provisioning
  workflow is deferred.

- **Q3: `LocationBinding` namespace choice (non-blocking).** The design assumes a single
  project namespace in the project control plane (e.g., `default`). If projects support
  multiple namespaces, should `LocationBinding` exist in each? Current compute resources
  all live in a single project namespace, so this is not blocking.

- **Q4: `LocationBindingReconciler` scalability for new PoPs (non-blocking).** When a new
  platform `Location` becomes `Ready`, the reconciler must create `LocationBinding` objects
  across potentially thousands of project control planes. This is a scatter-write. Phase 1
  can use a simple loop; evaluate work-queue fan-out if telemetry shows reconcile lag.

- **Q5: Quota allowances for `provider-dedicated` locations (non-blocking).** Should
  dedicated locations have separate quota allowance buckets from platform-managed locations,
  or should they count against the same per-location bucket? The dimension model supports
  either; this is a quota policy decision for the platform team, not a structural change.

- **Q6: Rename of `networking.datumapis.com/Location` to platform group (blocks Phase 4
  only).** Target API group (`platform.miloapis.com` vs `resourcemanager.miloapis.com`)
  needs a milo-level RFC. Does not block Phases 0-3.

### Implementation Notes

**For api-dev:**
- Add `ownerProjectRef` as an optional field to `LocationSpec`. It must be absent (or
  nil) for `datum-managed` locations and present for `provider-dedicated` and
  `self-managed` locations. A validating webhook should enforce this invariant.
- Add `locationClassName` as a typed string with kubebuilder enum validation rather than
  a free-form string. Valid values: `datum-managed`, `provider-dedicated`, `self-managed`.
- `LocationBinding.spec` should include `locationClassName` (copied from the parent
  `Location` at binding creation time) so consumers can filter without an additional lookup.
- `LocationBinding` CRD needs the `networking.datumapis.com/location` label index for
  efficient lookup by location name (needed by the `LocationBindingReconciler` fan-out).
- The `ServiceEntitlement` finalizer key must be
  `compute.datumapis.com/location-binding-controller`, not a generic name.
- **Project control plane mechanics**: Each Milo project has a virtual per-project API
  server endpoint. Resources are stored in shared Milo infrastructure but routed per
  project via `ProjectRouterWithRequestInfo` (`milo/pkg/server/filters/projects.go`).
  Controllers access a project's namespace through a per-project cluster client obtained
  from the multicluster-runtime manager.

- **`LocationBindingReconciler`** must use `mcmanager.Manager` (not standard
  `ctrl.Manager`). The Milo provider (`milo/pkg/multicluster-runtime/milo/provider.go`)
  automatically engages project clusters when a `Project` becomes `Ready`, making them
  available via `Manager.GetCluster(ctx, projectName)`. The reconciler registers with:
  ```go
  mcbuilder.ControllerManagedBy(mgr).
      For(&catalogv1alpha1.ServiceEntitlement{},
          mcbuilder.WithEngageWithLocalCluster(false),
          mcbuilder.WithEngageWithProviderClusters(true),
      ).
      Named("location-binding").
      Complete(r)
  ```
  This triggers reconciliation per-project whenever a `ServiceEntitlement` changes,
  using `req.ClusterName` to obtain the project-scoped client.

- **Writing `LocationBinding` objects**: Use `cluster.GetClient()` from
  `Manager.GetCluster(ctx, req.ClusterName)` to write into the project's default
  namespace. The same client is used to read the `ServiceEntitlement` and to list/delete
  existing `LocationBinding` objects for cleanup. This pattern is established by the quota
  `ResourceGrantController` at `milo/internal/quota/controllers/core/grant.go:43-77`.

- **Platform cluster reads**: The reconciler needs a separate client for the platform
  (core) cluster to read `Location` objects and `ServiceAvailability` objects. This is
  the local manager's client (`mgr.GetLocalManager().GetClient()`), or a dedicated
  platform cluster client injected at setup time.

- **Fan-out for new PoPs**: When a `Location` or `ServiceAvailability` changes in the
  platform cluster (not triggered by a per-project `ServiceEntitlement` event), the
  reconciler must iterate over all engaged project clusters using the multicluster
  manager's cluster list. This is the same scatter-write the quota system uses for
  `AllowanceBucket` creation. Phase 1 can use a simple iteration; evaluate work-queue
  fan-out if reconcile lag is observed at scale.

- **Three-gate check**: When determining which `Location` objects to project, apply all
  three gates: (1) class in `ServiceConfiguration.spec.locations.supportedClasses`,
  (2) `Location.status.conditions[Ready]=True`, (3) a `ServiceAvailability` object exists
  for the `(service, location)` pair with `status.conditions[Available]=True`.

- **`ServiceAvailability` creation**: The compute service operator must create a
  `ServiceAvailability` object in the `services` namespace of the platform control plane
  when it completes deployment and health validation at each PoP. Object name convention:
  `{service-name}--{location-name}` (e.g., `compute.datumapis.com--us-central1-a`).

**For test-engineer:**
- Test that activating a `compute.datumapis.com` entitlement creates `LocationBinding`
  objects for `datum-managed` locations only (class filter from `ServiceConfiguration`).
- Test that a new `datum-managed` `Location` becoming `Ready` after entitlement activation
  creates a `LocationBinding` in all projects with an active compute entitlement.
- Test that a `provider-dedicated` `Location` with `ownerProjectRef: acme-corp-project`
  creates a `LocationBinding` only in `acme-corp-project`, not in other projects.
- Test that deleting/deactivating the entitlement removes all
  `catalog.miloapis.com/service: compute.datumapis.com` labeled `LocationBinding` objects.
- Test that a `Location` transitioning to `Ready=False` sets
  `LocationBinding.status.conditions[Available]=False`.
- Test that the Workload webhook rejects city codes that map to no `Available=True`
  `LocationBinding` in the project namespace.
- Test that both `datum-managed` and `provider-dedicated` city codes are accepted when
  their bindings are `Available=True`.
- Test that `ResourceClaim` objects created by `InstanceReconciler` carry the
  `compute.datumapis.com/location` dimension populated from `Instance.status.location.name`
  for both platform-managed and dedicated locations.
- Test that a `Location` with `locationClassName: datum-managed` is rejected by the
  validating webhook if it includes `ownerProjectRef`.
