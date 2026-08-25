---
id: locations-platform-primitive
title: Locations as Platform Primitives for Service Consumers
status: draft
created: 2026-05-26
updated: 2026-08-24
author: Scot Wells
---

# Locations as Platform Primitives for Service Consumers

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

A design review on 2026-08-24 settled five questions this document had left open or had
answered differently. The API group moves to `locations.miloapis.com/v1alpha1` in a new
service, `LocationClass` becomes a real resource that owns provider configuration,
operational responsibility becomes its own field on `Location`, `LocationBinding` is
dropped as a kind in favour of a projected `Location`, and distribution reuses the
`SelfService` / `GatedByProvider` vocabulary the service catalog already has. Read
[Design Review Outcomes](#design-review-outcomes-2026-08-24) first: where the sections
below disagree with it, it wins, and each of those sections carries a pointer back to it.

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
- NFR4: The type must move from `networking.datumapis.com/v1alpha` to
  `locations.miloapis.com/v1alpha1`, served by the new `github.com/milo-os/locations`
  service, without a flag-day migration of all consumers simultaneously. The 2026-08-24
  review settled the target group; see
  [Design Review Outcomes](#design-review-outcomes-2026-08-24). This supersedes the
  earlier `resourcemanager.miloapis.com` target and the "stay in
  `networking.datumapis.com` for now" guidance below.
- NFR5: The core `Location` type must not name a cloud vendor in its schema. Provider
  configuration belongs to a provider-owned parameters resource that the location's class
  references, so that adding a provider is a new CRD in that provider's repository rather
  than a new field in the platform primitive.

---

## Design Review Outcomes (2026-08-24)

Five decisions from the design review. They are settled. Each records what it costs, and
each names the part of this document it replaces. Everything the review did not touch
stands as written, including the single-type recommendation (Decision 1), the class
selector model (Decision 3), the city-code API (Decision 4), the quota dimension
(Decision 5), and the three-gate model.

### D10: The API group is `locations.miloapis.com/v1alpha1`

`Location` and `LocationClass` move to `locations.miloapis.com/v1alpha1`, served by a new
service, `github.com/milo-os/locations`. This supersedes two things in this document: the
recommendation to stay in `networking.datumapis.com` and move only the Go definition, and
Decision 9's target of `resourcemanager.miloapis.com`.

Decision 9 argued that a location is a resource management primitive. The review agrees
with the diagnosis and rejects the destination. `resourcemanager.miloapis.com` is the
group that owns the project and organization hierarchy, and locations are not part of
that hierarchy. Folding them in makes the resource manager the home for anything that
does not fit elsewhere. A location has its own producers, its own lifecycle, and its own
controllers, so it gets its own group and its own service, the same way the service
catalog did.

The coordination cost is real and falls on `LocationBindingReconciler`, which is live.
That reconciler reads `Location` and writes `LocationBinding` as unstructured objects
against hard-coded `GroupVersionKind` constants, precisely because the typed Go types were
not available. That choice pays off here: moving the group is a change to those constants,
the `+kubebuilder:rbac` markers, and the set of CRDs installed into project control
planes. It is not a type migration. What it does cost is a period of dual reading during
the move, and the reconciler's only cross-plane trigger is a five-minute resync, so
expect up to five minutes of skew between the old and new objects while both are being
written. Nothing in the reconcile path breaks on that skew, because every write is an
idempotent upsert.

### D11: `LocationClass` is a resource, and provider configuration moves onto it

`locationClassName` stops being a free-form string validated by an enum and becomes a
reference to a real `LocationClass` resource. The class encodes capacity and provider
together: what kind of capacity this is and who supplies it. It carries two fields:

- `spec.controllerName`, naming the controller responsible for locations of this class.
  This follows `ConnectorClass.spec.controllerName` in network-services-operator and
  `GatewayClass.spec.controllerName` in Gateway API.
- `spec.parametersRef` (`{group, kind, name}`), pointing at a provider-owned parameters
  resource, GatewayClass-style.

`LocationSpec.Provider` and `GCPLocationProvider` are deleted from the core type. The GCP
`projectId`, `region`, and `zone` fields move into a parameters CRD owned by
`infra-provider-gcp`, referenced through `spec.parametersRef`.

The rationale is NFR5. A locations primitive should not encode a vendor type in its CRD
schema. `LocationProvider` with a `gcp` member means every new provider is a schema change
to the platform primitive, reviewed by the platform team, versioned with the platform
API, and installed everywhere even where it is meaningless. Behind a `parametersRef` a
provider ships its own CRD in its own repository on its own cadence.

The cost is one extra read on the reconcile path and one more object to keep alive.
Validation of the parameters resource is no longer the core CRD's job, which is why D11
comes with the `Accepted` condition recommendation in Q8 below.

```yaml
apiVersion: locations.miloapis.com/v1alpha1
kind: LocationClass
metadata:
  name: datum-gcp
spec:
  controllerName: infra-provider-gcp.datumapis.com/location-controller
  parametersRef:
    group: gcp.infra.datumapis.com
    kind: GCPLocationParameters
    name: datum-gcp-prod
```

### D12: Operational responsibility is its own field

`Location` gains `spec.operatedBy`, an enum with two values:

| `spec.operatedBy` | Meaning |
|-------------------|---------|
| `Platform` | The platform operates the control plane and the lifecycle of this location |
| `Consumer` | The consumer operates it; the platform records it and schedules to it |

Today `datum-managed` and `self-managed` describe who runs the control plane, while the
`provider` block on the same object describes who supplies the capacity. Those are
orthogonal, and fusing them into one class name produces a cross-product that has to be
enumerated by hand. A consumer-operated location whose capacity happens to sit in GCP is
expressible in the world and not expressible in the enum, and the moment a second provider
appears the enum needs `self-managed-gcp`, `self-managed-bare-metal`, and so on. Splitting
the axes means `LocationClass` answers "what capacity, from whom" and `operatedBy`
answers "who runs it", and neither has to enumerate the other.

`provider-dedicated` does not survive as a class value, and it does not become an
`operatedBy` value either, because it was never describing either axis. It described
exclusivity: who is allowed to use this location. That axis already has a field.
`ownerProjectRef` set means the location is dedicated to that project; absent means it is
shared. So the three existing values decompose:

| Today's `locationClassName` | `spec.classRef` | `spec.operatedBy` | `spec.ownerProjectRef` |
|-----------------------------|-----------------|-------------------|------------------------|
| `datum-managed` | a Datum-operated class, e.g. `datum-gcp` | `Platform` | unset |
| `provider-dedicated` | the provider's class | `Platform` | set |
| `self-managed` | the consumer's class, e.g. `self-managed-bare-metal` | `Consumer` | set |

This replaces the `locationClassName` Values table below and the
`LocationClassName` enum in `api/v1alpha1/serviceavailability_types.go`.
`ServiceConfiguration.spec.locations.supportedClasses` keeps its shape and its meaning
from Decision 3; its members become `LocationClass` names rather than enum members, which
is what makes the class name public API surface (Q13).

### D13: `LocationBinding` is dropped as a kind

The consumer-facing object is a `Location`, same group, same kind, projected into the
consumer's project control plane. There is no second type. Four reasons:

1. **The name misreads in this API surface.** `NetworkBinding.spec.location` in
   network-services-operator establishes that `XBinding` here means "X is bound to a
   location". `LocationBinding` therefore reads as "a location bound to a location".
2. **It is a replica, not a relationship.** The Kubernetes convention for `Binding` is an
   object that associates A with B: the core `Binding` object binds a Pod to a Node.
   `LocationBinding` associates nothing. It is a filtered copy of one `Location` with a
   condition on it.
3. **It repeats NFR3 one layer down.** NFR3 rejects separate resource types for
   consumer-owned and platform-managed locations, and Decision 1 grounds that rejection in
   Azure's `customLocations`. A distinct `LocationBinding` kind makes the same mistake
   against a different axis: the consumer's view of a location and the platform's view of
   the same location become two types with two schemas and two client code paths.
4. **The redaction argument is gone.** The stated justification for a reduced projection
   type was that provider-internal fields such as the GCP project ID and zone must not
   reach consumers. D11 removes those fields from `Location` entirely. There is nothing
   left on the object to withhold, so the projection can be the same kind.

The organizational precedent is `ProvisioningReconciler`, which applies producer-declared
objects into consumer control planes verbatim: same kind, same name, no projection type.
`LocationBindingReconciler` already keeps `metadata.name` equal to the `Location` name, so
the projected object is the same object under the same name in a different plane.

Everything that made `LocationBinding` work keeps working, because none of it depended on
the kind. The `Available` condition, the three gates, the controller owner reference to
the `ServiceEntitlement`, the label-scoped prune, and the five-minute resync are all
unchanged. The `spec.topology` map the compute workload webhook reads for city codes is
already mirrored verbatim from the `Location`, so after this change it is not mirrored at
all, it is simply present.

The projected `Location` is distinguishable from a platform `Location` by which control
plane it is in, and by the labels the reconciler already writes
(`services.miloapis.com/service-name`, and the managed-by label). It carries the same
`Available` condition, so a consumer still gets one list and one condition to read.

#### Migration from today's `LocationBinding` objects

The objects `LocationBindingReconciler` writes today are
`networking.datumapis.com/v1alpha` `LocationBinding` resources, cluster-scoped in project
control planes, owned by a `ServiceEntitlement`. Migrate in four steps, with no flag day:

1. **Install.** `milo-os/locations` publishes the `locations.miloapis.com/v1alpha1`
   `Location` and `LocationClass` CRDs, and the `Location` CRD is installed into project
   control planes alongside the existing `LocationBinding` CRD. Nothing reads the new
   objects yet.
2. **Dual-write.** `LocationBindingReconciler` writes both: the existing
   `LocationBinding` and a projected `Location` under the same name, same labels, same
   owner reference, same `Available` condition. Both are upserts against the same desired
   set, so the two stay consistent by construction, bounded by the five-minute resync.
   `cleanupBindings` gains the new kind and prunes both under the same owner and label
   scope.
3. **Move readers.** The compute workload webhook's city-code resolution, `datumctl
   compute locations list`, and the `NetworkBinding` path that reports
   `LocationNotAvailable` switch to listing projected `Location` objects. Each can move
   independently; during this step a reader on either kind sees the same answer. NSO's
   `NetworkBindingReasonLocationNotAvailable` reason string does not need to change, since
   it names a condition, not a kind.
4. **Stop and sweep.** The reconciler stops writing `LocationBinding`. Existing objects
   are removed by the prune that already exists: they carry a controller owner reference
   to the `ServiceEntitlement` and the managed-by label the prune selects on, so removing
   them from the desired set deletes them. Then uninstall the `LocationBinding` CRD from
   project control planes. `cleanupBindings` already returns cleanly on
   `apimeta.IsNoMatchError`, so a project whose CRD is gone before the sweep finishes does
   not error.

Nothing in this path requires a consumer to act at a particular moment, which satisfies
NFR4.

### D14: Distribution has two modes, `SelfService` and `GatedByProvider`

Location distribution reuses `EnablementMode` rather than inventing a parallel vocabulary:

- **`SelfService`** is today's derived behaviour, unchanged. An active
  `ServiceEntitlement`, a `ServiceAvailability` reporting `Available=True`, and a class in
  `ServiceConfiguration.spec.locations.supportedClasses` together produce the projected
  `Location`. The consumer requests nothing.
- **`GatedByProvider`** adds a consumer-initiated request. The consumer creates the
  request in their own control plane; the controller mirrors it into the producer's
  control plane under a deterministic hashed name; the provider writes
  `spec.approval.decision` and an optional `spec.approval.message`; on `Approved` the
  projection appears. This is exactly the `ServiceEntitlement` and `ServiceConsumer`
  mechanism, including the `sc-<hash>` naming derived from
  `sha256(serviceName + "/" + consumerProject)`, and it should share that machinery rather
  than reimplement it.

The request kind is **`LocationEntitlement`**, with `le-<hash>` as the mirrored name. The
name follows the house vocabulary: `ServiceEntitlement` is already the consumer-initiated,
provider-approved request object in this API, with the same three phases
(`PendingApproval`, `Active`, `Rejected`) and the same `spec.approval` handshake, so a
reader who knows one knows the other. `LocationRequest` was the runner-up and was rejected
for describing the object at the moment it is created rather than for its whole lifetime;
an approved `LocationRequest` is no longer a request.

It is explicitly not called `LocationBinding`, for the D13 reasons plus one more: it would
name the request object after the thing the request produces, which is the projected
`Location`.

One asymmetry to note. `ServiceEntitlement` and `ServiceConsumer` are two kinds because
the consumer must not see other consumers' records and the provider must not write into
the consumer's plane. `LocationEntitlement` as described mirrors one kind across both
planes and relies on the mirrored copy being in the producer's plane for that isolation.
Whether it needs a distinct producer-side kind for RBAC symmetry is a question for
implementation, not a reason to change the name.

### What this replaces in the sections below

| Section | Status |
|---------|--------|
| Recommendation: Single `Location` Type | Group guidance replaced by D10; the single-type conclusion stands and D13 strengthens it |
| `locationClassName` Values | Replaced by D11 and D12 |
| Project-Level `LocationBinding` (new resource) | Replaced by D13 |
| Platform-Level `Location`, and all `Location` examples | `spec.provider` replaced by D11; `spec.locationClassName` replaced by `spec.classRef` and `spec.operatedBy` |
| Two-Tier Discovery Model, Three-Gate Model, lifecycle, Consumer-Facing API | Mechanism unchanged; every `LocationBinding` reads as "projected `Location`" |
| Security Considerations | The "provider-internal fields are never copied" argument is moot under D11; the fields are not on `Location` |
| Decision 9, Migration Path phase 4 | Replaced by D10 |
| Q2, Q6 | Answered by D14 and D10 respectively |

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

> **Amended by D10 and D11.** The group is `locations.miloapis.com/v1alpha1`, served by
> `github.com/milo-os/locations`, and the move is no longer deferred to a separate RFC.
> `locationClassName` is not a well-known string set; it is a reference to a
> `LocationClass` resource. The paragraph's conclusion that one `Location` type serves all
> variants is unchanged, and D13 extends it: the consumer's view is that same type too.

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

> **Replaced by D11 and D12.** These three values are not one axis. `LocationClass` is now
> a resource covering capacity and provider, `spec.operatedBy` covers who runs the
> location, and `spec.ownerProjectRef` covers exclusivity. See the decomposition table in
> D12 for how each value maps. New classes no longer require a platform-level enum change
> at all; a provider creates a `LocationClass`.

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

> **Unchanged by the review, except for naming.** Under D13 the object the gates resolve
> onto is the projected `Location`, not a `LocationBinding`. Gate 1 still reads
> `supportedClasses`; its members are now `LocationClass` names.

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

> **Amended by D13.** The consumer-facing API is a `Location` projected into the project
> control plane, not a `LocationBinding`. The two-tier structure and the sentence about
> querying one resource type are unchanged; under D13 that one resource type is now the
> same one the platform uses.

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

Locations in the platform control plane are managed by platform operators.

> **Amended by D10 and D11.** The structure does change. `apiVersion` becomes
> `locations.miloapis.com/v1alpha1`, `spec.locationClassName` becomes `spec.classRef`
> naming a `LocationClass`, `spec.operatedBy` is added, and the `spec.provider` block in
> the example below is deleted from the type. Its GCP fields live in an
> `infra-provider-gcp` parameters CRD that the class references. Read the example for the
> topology and condition shape, which are unchanged, not for the group or the provider
> block.

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

> **Replaced by D13.** `LocationBinding` is dropped as a kind. The consumer-facing object
> is a `Location` in the project control plane, same group, same kind, same name. The
> reduction argument in the paragraph below no longer holds: D11 removes the
> provider-internal fields from `Location`, so there is nothing to withhold. Read this
> section for the labels, the `Available` condition, and the gate-closure reasons, all of
> which carry over verbatim onto the projected `Location`.

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

> **Amended by D13.** Option C is still the right answer against A and B: the objection to
> Option A was storage explosion and update fan-out, and a projection solves both. What
> D13 changes is that the projection is a `Location`, not a new kind. Option A was
> rejected for copying *full* location specs including provider config into every project;
> after D11 there is no provider config to copy, so the projected object is already the
> thin thing Option C wanted, without a second schema.

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

> **Amended by D12 and D14.** Read `spec.locationClassName: self-managed` as
> `spec.operatedBy: Consumer` with a consumer-supplied `LocationClass`, and
> `provider-dedicated` as `spec.operatedBy: Platform` with `ownerProjectRef` set. The four
> control points themselves are unchanged, and D14 gives the third one a concrete
> mechanism: a `GatedByProvider` `LocationEntitlement` approved by the provider.

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

> **Amended by D12, D13, and D14.** The class column decomposes per the D12 table, the
> object created is a projected `Location`, and under `GatedByProvider` the trigger is an
> approved `LocationEntitlement` rather than the bare existence of a `Location` with
> `ownerProjectRef` set. The observation that one reconciler can serve all rows still
> holds.

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
| `LocationReconciler` | `milo-os/locations` (D10) | Manages platform `Location` objects; sets `Ready` condition (gate 2) |
| `LocationClassReconciler` | `milo-os/locations` (D11) | Resolves `spec.parametersRef`; sets the `Accepted` condition (Q8) |
| `ServiceAvailabilityReconciler` | `milo-os/service-catalog` | Created by each service operator when deployed and validated at a PoP; sets `Available` condition (gate 3). See milo-os/service-catalog#24. |
| `LocationBindingReconciler` | `milo-os/service-catalog` | Watches all three gates; creates and maintains the projections in project control planes; handles fan-out for new PoPs; handles entitlement deletion cleanup. Under D13 it projects `Location` objects; rename it `LocationProjectionReconciler` when the `LocationBinding` write is retired |
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

> **Amended by D10 and D13.** The underlying resource is a projected `Location` and the
> CLI calls `GET /apis/locations.miloapis.com/v1alpha1/locations` against the project
> control plane. The `CLASS` column becomes `spec.classRef.name`, and an `OPERATED BY`
> column surfaces `spec.operatedBy`. The list itself, one filtered view of what the
> project can use, is unchanged.

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

> **Moot under D11.** Those fields are not on `Location` any more. They live in a
> provider-owned parameters resource in the platform control plane, referenced by
> `LocationClass.spec.parametersRef`, and are protected by that resource's own RBAC rather
> than by a projection that omits them. The class name itself remains visible to consumers
> through `spec.classRef`, which is why Q13 treats it as public API surface.

---

## Migration Path

| Phase | Current state | Target state | Action |
|-------|--------------|--------------|--------|
| 0 (now) | `Location` type in NSO; webhook lists all platform locations globally | Shared type module with `ownerProjectRef` field | Extract type to shared module; add `ownerProjectRef` field and `locationClassName` enum; no behavior change |
| 1 | Webhook uses cluster-wide `Location` list; no per-project visibility | `LocationBinding` per project; webhook uses project-scoped list; three-gate model enforced | Implement `ServiceAvailability` CRD + `LocationBindingReconciler` in `milo-os/service-catalog`; compute service operator creates `ServiceAvailability` on each PoP deployment; update webhook to use `LocationBinding` list; add `locations` section to compute `ServiceConfiguration` |
| 2 | No location dimension on `ResourceClaim`; no dedicated location support | Location dimension populated on all new claims; `provider-dedicated` bindings created on `Location` creation | Update `InstanceReconciler` claim construction; `LocationBindingReconciler` handles non-`datum-managed` classes via `ownerProjectRef` |
| 3 (future) | `self-managed` location registration is manual | Consumer-facing registration workflow | Consumer-facing API and automation for registering self-managed sites; may include a `LocationRegistration` request resource |
| 4 | `networking.datumapis.com/v1alpha` `Location` and `LocationBinding` | `locations.miloapis.com/v1alpha1` `Location` and `LocationClass` in `milo-os/locations`; no `LocationBinding` | D10 and D13. Stand up `milo-os/locations`; install both CRDs; dual-write the projected `Location` beside the existing `LocationBinding`; move readers; stop writing `LocationBinding` and let the existing owner-scoped prune sweep it; uninstall the CRD. Steps are detailed under D13 |
| 5 | `LocationSpec.Provider` with a `gcp` member; `locationClassName` as an enum | `LocationClass` resource with `controllerName` and `parametersRef`; `spec.classRef` and `spec.operatedBy` on `Location` | D11 and D12. `infra-provider-gcp` ships its own parameters CRD and a `LocationClass` per offering; delete `LocationProvider` and `GCPLocationProvider` from the core type; replace the `LocationClassName` enum in `serviceavailability_types.go` with a name reference |
| 6 | Projection is always derived from the entitlement | `SelfService` derived projection plus a `GatedByProvider` `LocationEntitlement` request flow | D14. Reuse the `ServiceEntitlement` and `ServiceConsumer` mirroring and `spec.approval` handshake rather than reimplementing it |

Phase 4 supersedes the earlier "future, separate RFC" framing of the group move: it is
now a planned phase with a concrete dual-write path, and it does not block Phases 1
through 3, which can be built against the current group and carried across by the
dual-write. Phase 5 depends on Phase 4, because the class reference and `operatedBy` are
introduced on the new type rather than retrofitted onto the old one. Phase 6 depends on
Phase 5, because the gated flow approves access to a class.

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

- **Decision 9: API group target is `resourcemanager.miloapis.com`.** ~~`Location` is a
  resource management primitive: it describes where platform resources exist and are
  accessible.~~ **Superseded by Decision 10.** The diagnosis stands: `Location` is not a
  networking primitive, and `networking.datumapis.com` is wrong for the same reason
  `Microsoft.ExtendedLocation` is confusing. The destination changed.
  `resourcemanager.miloapis.com` owns the project and organization hierarchy, and a
  location is not part of that hierarchy.

The five decisions below were settled in the 2026-08-24 design review. They are stated in
full, with their tradeoffs, under
[Design Review Outcomes](#design-review-outcomes-2026-08-24).

- **Decision 10: The API group is `locations.miloapis.com/v1alpha1`, served by
  `github.com/milo-os/locations`.** A location has its own producers, lifecycle, and
  controllers, so it gets its own group and its own service rather than being folded into
  the resource manager. Supersedes Decision 9 and the "stay in `networking.datumapis.com`
  for now" recommendation. Cost: a dual-write window against the live
  `LocationBindingReconciler`, cheap because that reconciler already reads and writes
  these objects unstructured against hard-coded GVK constants.

- **Decision 11: `LocationClass` is a resource carrying `spec.controllerName` and
  `spec.parametersRef`; provider configuration leaves `Location`.** The class encodes
  capacity and provider. `LocationSpec.Provider` and `GCPLocationProvider` are deleted from
  the core type and `infra-provider-gcp` owns its own parameters CRD. Rationale: a
  locations primitive should not encode a vendor type in its CRD schema, and behind a
  `parametersRef` a provider ships its own CRD on its own cadence. Cost: one extra read on
  the reconcile path, and validation moves out of the core CRD, which is what Q8 addresses.

- **Decision 12: Operational responsibility is `spec.operatedBy` (`Platform` |
  `Consumer`), not part of the class name.** Who runs the location and who supplies its
  capacity are orthogonal; fusing them produces a class cross-product that has to be
  enumerated by hand, and leaves a consumer-operated location on GCP inexpressible.
  `provider-dedicated` does not survive on either axis, because it described exclusivity,
  which `ownerProjectRef` already carries.

- **Decision 13: `LocationBinding` is dropped as a kind; the consumer-facing object is a
  `Location` projected into the consumer's project control plane.** Four reasons: in this
  API surface `XBinding` means "X is bound to a location" (`NetworkBinding.spec.location`),
  so the name misreads; it is a replica rather than a relationship object, which breaks the
  Kubernetes convention that `Binding` associates A with B; NFR3 already rejects a separate
  type for consumer-owned versus platform-managed and a distinct kind is that same mistake
  one layer down; and Decision 11 removes the provider fields that justified a reduced
  projection type. Precedent: `ProvisioningReconciler` applies producer-declared objects
  into consumer control planes verbatim, same kind, same name.

- **Decision 14: Distribution is `SelfService` or `GatedByProvider`, reusing
  `EnablementMode`.** `SelfService` is today's derived behaviour. `GatedByProvider` adds a
  consumer-initiated `LocationEntitlement`, mirrored into the producer's control plane
  under a deterministic `le-<hash>` name and approved through
  `spec.approval{decision,message}`, sharing the `ServiceEntitlement` and `ServiceConsumer`
  machinery rather than reimplementing it. The request kind is explicitly not called
  `LocationBinding`.

### Open Questions

- **Q1: `ServiceConfiguration.spec.locations` topology selectors (non-blocking).** The
  design reserves a `topologySelectors` field in `ServiceConfiguration.spec.locations` for
  restricting the service to specific regions within a supported class (e.g., a
  configuration version that only covers `us-central1` initially). Should this be in scope
  for Phase 1 or deferred until there is a concrete use case? Proceed without topology
  selectors for Phase 1 — all `Ready` locations of a supported class are projected.

- **Q2: `provider-dedicated` location provisioning workflow.** **Answered by Decision
  14.** The workflow is a control plane API: a consumer-initiated `LocationEntitlement`
  under `GatedByProvider`, approved by the provider through `spec.approval`. No
  `LocationProvisioningRequest` resource is needed. Phase 2 can still proceed with direct
  `Location` creation by platform operators; Phase 6 delivers the request flow.

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

- **Q6: Rename of `networking.datumapis.com/Location` to platform group.** **Answered by
  Decision 10.** The group is `locations.miloapis.com/v1alpha1` in a new service. Neither
  `platform.miloapis.com` nor `resourcemanager.miloapis.com`.

The questions below are open. Each carries a recommendation, which is a starting position
for the next review, not a decision.

- **Q7: `parametersRef` versus an inline `RawExtension` on `LocationClass`.** Decision 11
  settles that provider configuration leaves `Location`; it does not settle how the class
  carries it. There is an in-house counter-precedent for inlining:
  `ServiceConfiguration.spec.provisioning.resources[].objects` embeds whole objects as
  `runtime.RawExtension` with `+kubebuilder:validation:EmbeddedResource` and
  `+kubebuilder:pruning:PreserveUnknownFields`, and `ProvisioningReconciler` decodes and
  applies them. That works there because the objects are opaque payloads the platform
  never interprets and the target API validates on write.

  **Recommendation: the reference.** Provider parameters are not opaque payloads; the
  provider's own controller reads them and should have them validated by a real schema at
  admission rather than discovering a typo at reconcile time. A separate object also gets
  separate RBAC, so credentials-adjacent configuration is not readable by everyone who can
  read a `LocationClass`, and a separate lifecycle, so parameters can be rotated without
  touching the class. And it keeps `x-kubernetes-preserve-unknown-fields` out of a core
  platform CRD, where it disables server-side pruning and defaulting for the subtree.

- **Q8: How `LocationClass` surfaces a parameters resolution failure.**
  **Recommendation: a GatewayClass-style `Accepted` condition**, with reason
  `InvalidParameters` when `spec.parametersRef` does not resolve, resolves to the wrong
  kind, or fails the provider controller's own validation. This composes with the habit
  the catalog already has: `ProvisioningReconciler` records `Unprovisionable` on the
  entitlement ledger with a message naming the service and the resource rather than
  skipping the declaration silently, on the stated grounds that the consumer has to be
  told. A class that cannot resolve its parameters should be visibly not `Accepted`, not
  quietly absent from projections.

- **Q9: Deletion ordering between a parameters object and the locations that use it.**
  A deleted parameters object must not withdraw a location that cells are actively
  serving. **Recommendation: deleting the parameters resource marks the `LocationClass`
  not `Accepted` and blocks new provisioning only.** Existing locations of that class keep
  their `Ready` condition and keep serving; the class stops being eligible for new
  projections and new `Location` objects referencing it are refused. This mirrors the
  catalog's existing removal guard, where a `Service` cannot be deleted while meters or
  monitored resource types still reference it: the reference is what blocks, and the
  running system is never withdrawn out from under its users by a delete.

- **Q10: The class reference must carry a project qualifier.** A projected `Location` in
  the consumer's control plane names a `LocationClass` that lives in the producer's
  control plane, so a bare `name` does not identify it. `spec.classRef` needs the producer
  project alongside the name. `milo-os/ipam` hit the same shape with `IPClass` and had to
  fold the consuming project into pool identity to disambiguate; the lesson is that a
  cross-plane class reference is not a name, it is a name plus a plane, and deciding that
  late is expensive. Recommendation: make `classRef` `{name, projectRef}` from the start,
  even while there is only one producer.

- **Q11: Projected `Location` name collisions.** `LocationBindingReconciler` today sets
  `metadata.name` to the `Location` name, and projected `Location` objects are
  cluster-scoped in the project control plane. One consumer subscribed to two producers
  that both offer `us-east-1` gets a collision, and the current upsert would have the two
  reconcile loops fight over one object. Options: qualify the projected name by producer
  (a hash prefix, as `sc-<hash>` already does elsewhere), or keep the friendly name and
  reject the second projection with a visible condition. Recommendation: qualify the name
  and carry the producer's chosen name in a label or `spec.displayName` for the CLI to
  render, because a silent last-writer-wins here is the failure mode that is hardest to
  see. This interacts with Q13: if the name is hashed, the class name carries even more of
  the human-facing identity.

- **Q12: Who may create a `LocationClass`.** A class references provider credentials
  through `spec.parametersRef` and names a controller that will act on them, so creating a
  class is close to granting capacity. Candidates: platform operators only; the producer
  project that owns the referenced parameters; or a producer with a platform approval step
  reusing the Decision 14 handshake. Recommendation: scope create to the producer project
  that owns the parameters resource, and let the `Accepted` condition from Q8 be the
  platform's control point, so an unapproved class exists but projects nothing.

- **Q13: The class name is public API surface.** A consumer reads `spec.classRef` on a
  projected `Location` but cannot read the `LocationClass` itself: it is in another
  control plane behind another IAM boundary. The name is therefore the entire
  human-readable description of what that location is, and it appears in
  `ServiceConfiguration.spec.locations.supportedClasses`, in `datumctl` output, and in
  support conversations. Recommendation: treat class names as immutable once used and
  document a naming convention before the first provider ships one, rather than letting
  the first `LocationClass` set the precedent by accident.

### Implementation Notes

**For api-dev:**

> **Amended by D10 through D14.** These notes predate the review. `LocationBinding` is a
> projected `Location`; `locationClassName` is `spec.classRef` plus `spec.operatedBy`;
> there is no `LocationClassName` enum to add; the group is
> `locations.miloapis.com/v1alpha1`. The multicluster mechanics, the finalizer, the
> label-index requirement, and the three-gate check carry over unchanged, and so does the
> `ownerProjectRef` invariant, restated as: `ownerProjectRef` is set exactly when the
> location is dedicated to one project, which is every `operatedBy: Consumer` location and
> those `operatedBy: Platform` locations a provider dedicates.

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
