---
id: project-service-availability
title: A Service-Agnostic Location Projection and Per-Project Service Availability
status: draft
created: 2026-08-29
updated: 2026-08-29
author: Scot Wells
---

# A Service-Agnostic Location Projection and Per-Project Service Availability

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Open Questions](#open-questions)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)

## Summary

A project entitled to a single service can today discover, from inside its own
control plane, which Locations that service works at. A project entitled to
**two** services cannot: the second service's projection fails, permanently.
This document proposes fixing that by separating two questions that are
currently fused onto one object — which Locations a project can use at all, and
which of the project's entitled services actually work at each of them — and by
making the answer to the second question directly readable from inside a
project's own control plane, which it is not today.

The consumer-facing object here is `Location`
(`locations.miloapis.com/v1alpha1`), projected into an entitled project by this
repo's `LocationBindingReconciler`. `LocationBinding` is written alongside it
only until its remaining readers move off; it is not the object this design
builds on. See [LocationBinding's retirement](#locationbindings-retirement).

This extends [Locations as Platform Primitives for Service
Consumers](./locations-platform-primitive.md), which defined the three-gate
availability model these projections read. It does not revisit that model; it
scopes how per-project visibility into it should work.

## Motivation

A projected `Location` is the object a project's own control plane holds so a
consumer can discover, without leaving their project, which Locations they can
use. It is written once per (consumer project, `ServiceEntitlement`) by
`LocationBindingReconciler`. That per-entitlement design was fine when a project
held a single entitlement. It breaks as soon as a project holds two.

**A second entitlement fails hard, permanently, on every retry.** The projected
object is named by Location alone (`u.SetName(locName)`), carries a
`services.miloapis.com/service-name` label recording which service produced it,
and is Kubernetes-owned by the one `ServiceEntitlement` whose reconcile happened
to create it first (`controllerutil.SetControllerReference`, in
`upsertProjection`). A second service's entitlement, reconciling independently,
tries to take ownership of the same object and is rejected:

```
failed to upsert Location "us-central1-a": set controller reference:
  Object /us-central1-a is already owned by another ServiceEntitlement
  controller compute
```

This was reproduced directly against the reconciler on `main`. Whichever
entitlement reconciles first wins; every other service entitled in that project
never gets its Locations projected, and the failure does not resolve itself on
retry — it is deterministic on object identity, not a transient race. Nothing
about today's single-entitlement deployments exercises this path, so it has
shipped unnoticed; it fires the first time any project holds a second
entitlement. Because both projection kinds are written through the same
`upsertProjection` path, `LocationBinding` fails the same way.

**Even where it doesn't fail outright, the object can't express the answer a
customer actually needs.** The projection folds three separate facts into one
`Available` condition, computed in `evaluateGates`: whether the service's
published configuration supports this Location's class, whether the Location is
Ready, and whether the service has separately confirmed it is operational there
(a `ServiceAvailability` with `Available=True`, which is what makes the
projection exist at all). The first and third of those are per-service facts.
Keyed by Location alone, one object cannot represent "Compute is available in
Dallas, but the AI gateway is not" — the case the three-gate model in
[locations-platform-primitive.md](./locations-platform-primitive.md) was built
to represent for the platform as a whole is currently impossible to see from
inside a single project once that project uses more than one service.

**And there's no way to see the per-service verdict from inside a project at
all today**, independent of the bug above. `ServiceAvailability` — the object
that records "this service is confirmed live at this Location" — is
cluster-scoped in the root key space, read by the reconciler through
`rootClient`. Its `ProtectedResource`
(`config/components/iam/protected-resources/serviceavailability.yaml`) declares
no `parentResources`, and the type is annotated
`discovery.miloapis.com/parent-contexts=Platform`. An ordinary project member
has no IAM path to read it and no copy of it in their own control plane.

### Goals

- Fix the failure: any number of entitled services in the same project must be
  able to project Locations without one clobbering another.
- Let a project see, for each service it's entitled to, exactly which of its
  visible Locations that service actually works at — not just an aggregate
  "some Location works for something" signal.
- Keep discovery inside the project's own control plane, consistent with every
  other Milo consumer-facing resource — no new cross-project read path.
- Show a service's availability only for services the project actually holds an
  active entitlement for.

### Non-Goals

- Revisiting the three-gate availability model itself, or the `Location`
  primitive, both defined in
  [locations-platform-primitive.md](./locations-platform-primitive.md). This
  document is scoped to per-project visibility into gates already defined there.
- A public catalog surface for browsing availability across services a project
  has *not* entitled (see [Leakage](#leakage)).
- Driving `LocationBinding`'s retirement, which depends on readers outside this
  repo. See [LocationBinding's retirement](#locationbindings-retirement).
- A migration plan for `edge.datum.net` or any other pre-existing location
  surface — out of scope of the referenced document and unchanged here.

## Proposal

Split what is today one object and one reconcile path into two:

| Question | Today | Proposed |
|---|---|---|
| Which places can this project use at all? | A projected `Location` per (project, entitlement, location) — breaks past one entitlement | A projected `Location` per (project, location), service-agnostic |
| Which of the project's services work at each place? | Not visible from inside a project | A per-(service, location) record, mirrored into the project for each entitled service |

The projected `Location` becomes what its name and its consumer-facing behavior
already imply it should be: a place the project can use, independent of which
service asked for it. It exists because *some* active entitlement in the project
reaches it, and carries the platform Location's own service-agnostic facts —
its class, topology, coordinates, and, once
[#85](https://github.com/milo-os/service-catalog/pull/85) lands, the platform
Location's own status conditions. It stops carrying a single per-service
verdict.

`ServiceAvailability` — gate 3 — is mirrored per (service, Location) into every
project holding an active entitlement for that service. A project entitled to
Compute and Object Storage, at a Location where only Compute has shipped, sees
`Location/us-east-1` (a usable place) and a Compute availability record for it,
but no Object Storage record. The absence is the signal, exactly as the
platform-scoped model already establishes in
[locations-platform-primitive.md](./locations-platform-primitive.md).

### User Stories

#### Story 1: A project entitled to two services can see both

Acme's project holds active entitlements for Compute and Object Storage. Today,
only whichever service's entitlement reconciled first successfully projects any
Locations at all; the other silently gets none, forever. Under this proposal,
both entitlements' Locations are visible, and Acme can further tell, per
Location, which of the two services is actually confirmed running there.

#### Story 2: A service is healthy at a Location, another isn't — visible from inside the project

At `us-east-1`, Compute is confirmed live; Object Storage hasn't shipped there
yet. From inside Acme's own project, `Location/us-east-1` shows the place is
usable, and an availability record exists for Compute at that Location but not
for Object Storage. Acme does not need platform access, or a support ticket, to
learn this distinction — today they have no way to see it at all from their own
project.

#### Story 3: A service Acme has never entitled stays invisible

Acme's project has no entitlement for the AI Gateway service. Nothing about AI
Gateway's availability — anywhere — is mirrored into Acme's project. The absence
is total, not filtered client-side: the object is never created in Acme's
control plane in the first place.

### Notes/Constraints/Caveats

Everything from here down is mechanism.

#### Ownership and garbage collection

This is the central design question and deserves the most scrutiny in this
document.

**Why the current model breaks.** `LocationBindingReconciler` reconciles once
per `ServiceEntitlement`, and each reconcile independently decides the full set
of projections *that entitlement* wants, using
`controllerutil.SetControllerReference` in `upsertProjection` to make that one
entitlement the Kubernetes-native, garbage-collecting owner. Ownership by
exactly one controller is a hard Kubernetes invariant, so a second entitlement's
reconcile cannot become a second owner of the same object and fails outright.
Once the projection stops being per-service, no single entitlement is the right
owner: the object's existence should depend on *whether any* active entitlement
in the project still needs it, not on which one happened to create it.

**Proposed model: reconcile the project, not the entitlement.** Change the unit
of reconciliation from "one `ServiceEntitlement`" to "the project as a whole." A
reconcile is still triggered by a `ServiceEntitlement` watch event, but the
reconcile body lists every *Active* `ServiceEntitlement` in that project cluster
(`req.ClusterName`), computes the full desired set of projections across all of
them, upserts everything desired, and prunes anything the reconciler manages
that is no longer in that set.

This is a generalization of a pattern already in the codebase.
`cleanupProjections` already does mark-and-sweep pruning against a keep-set —
it lists by the `managedByFanoutSelector` label selector and then narrows to one
entitlement's objects via `ownedBy(item.GetOwnerReferences(), entitlementUID)`.
`QuotaFanOut` and `BillingFanOut` apply and prune fan-out sets the same way.
Widening the keep-set from "one entitlement's desired Locations" to "the union
of every active entitlement's desired Locations" removes the ownership conflict
entirely, because no per-entitlement `SetControllerReference` call happens
anymore.

Concretely:

- Projections are no longer Kubernetes-owned by a `ServiceEntitlement`. They
  keep the `app.kubernetes.io/managed-by: services.miloapis.com` label the
  prune selector already matches, and garbage collection is driven entirely by
  the reconciler's desired-state computation rather than the Kubernetes GC
  cascade. `cleanupProjections` drops its `ownedBy` narrowing and prunes on the
  label selector alone, project-wide.
- The `Reconcile` fast-path that returns on a `NotFound` entitlement — today
  correct only because the projections carry that entitlement's owner reference
  and are GC'd — must instead fall through to the project-wide recompute. Same
  for the non-Active and empty-`supportedClasses` branches, which today call
  `cleanupProjections(..., nil)` to tear down one entitlement's projections and
  must become "recompute the project without this entitlement's contribution."
- Deleting the last entitlement in a project prunes every managed projection
  there, as the project-wide recompute of an empty Active set.
- The existing `locationBindingResyncInterval` resync (5 minutes, covering
  root-cluster gates that multicluster-runtime cannot watch into a
  project-scoped request) doubles as a self-healing pass, so a dropped watch
  event does not leave a stale or under-provisioned projection indefinitely.
- The same model applies to the `ServiceAvailability` mirror: desired state per
  project is "one mirrored record per (service, Location) where the project
  holds an Active entitlement for that service and a matching platform
  `ServiceAvailability` exists," recomputed and pruned the same way.

**Alternative considered: multiple non-owning `ownerReferences`.** Kubernetes
supports attaching more than one `ownerReference` as long as at most one is
marked as the controller, and native garbage collection deletes the dependent
once *none* of its owners still resolve — reference-counted cleanup for free.
Set aside for this document: it would be a second garbage collection mechanism
alongside the mark-and-sweep pruning this codebase already uses everywhere else,
and correctness would depend on Kubernetes GC cascade behavior rather than
reconciler logic that is easy to read, test, and reason about locally. Worth
revisiting if project-wide recompute proves too expensive at high entitlement or
Location counts — see [Open Questions](#open-questions).

**Project deletion** is out of scope for this cleanup logic. A project's entire
control plane and key space go away when Milo deletes the project, taking every
object stored there with it. No finalizer-driven cleanup is needed, consistent
with how `ServiceEntitlement` already relies on the project's own lifecycle.

#### IAM

The projected `Location` needs no new IAM work: `milo-os/locations` already
ships a project-scoped `ProtectedResource` for
`locations.miloapis.com` `Location` with

```yaml
parentResources:
  - apiGroup: resourcemanager.miloapis.com
    kind: Project
```

and a `locations.miloapis.com-location-viewer` role granting list/get/watch.

The mirrored `ServiceAvailability` is the gap. It is a new consumer-visible kind
inside a project's control plane, and it needs its own registration or it is
invisible even once it exists. As noted above, the existing `ProtectedResource`
declares no `parentResources` and the type is annotated for the `Platform`
discovery context only, so an ordinary project member has no IAM path to it
today. Mirroring the object without also registering it as a project-scoped
resource would produce an object nobody entitled to see it can read.

The fix follows the same pattern: a second, project-scoped `ProtectedResource`
for the mirrored copy with a `resourcemanager.miloapis.com` `Project` parent,
plus a viewer role — either a new one or an addition to
`services.miloapis.com-entitlement-viewer`, which already covers project-scoped
`ServiceEntitlement` and `ServiceConsumer`. The platform-scoped
`ProtectedResource` is unaffected and continues to gate the root copy.

#### Leakage

The availability matrix — which services exist and where they've shipped — is
not necessarily something every customer should be able to enumerate, and a
project's control plane is a strict isolation boundary elsewhere in Milo. This
design mirrors a `ServiceAvailability` into a project only for a service the
project holds an *Active* entitlement for — the same restriction the projection
path already applies today, generalized across every entitled service instead of
the single one it currently assumes. No aggregate or cross-project endpoint is
introduced; a project can only ever see availability for services it has itself
entitled.

#### Object count

Per project, the count is bounded by (active entitlements) × (Locations that
service is available at) for the mirrors, plus one `Location` per distinct
Location visible to any of those entitlements. That is usage-bound, not
catalog-bound: a project's object count tracks what it uses, not the size of the
platform's service or Location catalog — the same shape the projection already
has today. This should not need bounding at current scale. If the Location
catalog grows into the hundreds and projects routinely hold many entitlements,
the mirror set should be revisited, but nothing here suggests that is imminent.

#### Migration

The projected `Location` keeps its name and identity under this proposal; only
the ownership model changes underneath it, through the same
`controllerutil.CreateOrUpdate` upsert already in use. Existing objects are
updated on their next reconcile, not recreated. No dual-write, no
customer-visible cutover, no coordination with consumers.

One migration step is not free, and the naive version of it is a data-loss bug:
the stale controller `ownerReference` must be **explicitly removed** in the
mutate function. `CreateOrUpdate` does not clear metadata the mutate function
does not touch, so an owner reference left behind means deleting that one
entitlement still cascades a delete of projections other entitlements need. The
implementation must strip any `ServiceEntitlement` owner reference on the way
through, and that should be covered by a test that deletes the first-created
entitlement and asserts the projection survives while a second entitlement still
wants it.

Sequencing:

1. [#85](https://github.com/milo-os/service-catalog/pull/85) first. It is
   additive, does not touch ownership, and lands in the same functions this
   design restructures (`upsertProjection`, and `setProjectionAvailable`, which
   it renames to `setProjectionStatus`). Rebasing it onto an ownership rewrite
   is strictly more work than the reverse.
2. The ownership fix on its own. It is a pure bugfix for something already
   broken the moment a project holds two entitlements, independent of everything
   else here.
3. The `ServiceAvailability` mirror and its IAM registration, together. Additive
   and separable: a project's `Location` visibility is not conditioned on the
   mirror existing.

#### LocationBinding's retirement

`LocationBinding` is being retired. `locationBindingGVK`'s comment in the
reconciler is explicit that it is written only until the network-services
operator's remaining readers move off it, and
[locations-platform-primitive.md](./locations-platform-primitive.md#migration-off-locationbinding)
tracks the reader set. This document adds nothing to it and does not extend it.

Under this design it inherits the ownership fix automatically, because both
kinds are written through the same `project` / `upsertProjection` /
`cleanupProjections` path — which it needs, since it fails on a second
entitlement exactly as the `Location` projection does. It gains no new fields
and is not made a target of the `ServiceAvailability` mirror. When the last
reader moves, dropping it from `projectionGVKs` removes it from this design with
no other change.

The precise reader set is in flux and this document does not attempt to pin it:
`locations-platform-primitive.md` names four readers, and the
`NetworkPresence` controller the reconciler's comment cites is not on
`network-services-operator`'s `main` as of this writing. Treat that table, not
this document, as the source of truth.

### Risks and Mitigations

- **Risk:** Recomputing the full desired set for every entitlement in a project
  on every reconcile is more work per reconcile than today's single-entitlement
  pass.
  **Mitigation:** Bounded by the number of active entitlements in one project,
  which is small in practice; the existing 5-minute resync already assumes a
  full-recompute cost model for the cross-cluster gates.
- **Risk:** Dropping Kubernetes-native controller ownership means garbage
  collection depends entirely on the reconciler's mark-and-sweep logic being
  correct, rather than on a platform-enforced cascade.
  **Mitigation:** Not a new pattern here — `cleanupProjections`, `QuotaFanOut`,
  and `BillingFanOut` already work this way. The change is the scope of the
  desired-set computation, not the mechanism.
- **Risk:** A stale controller `ownerReference` surviving migration cascades a
  delete of projections other entitlements still need.
  **Mitigation:** Explicit removal in the mutate function plus the survival test
  described in [Migration](#migration).
- **Risk:** A mirrored `ServiceAvailability` without correct IAM registration
  ships invisible, and nothing errors — the object simply exists with no reader.
  **Mitigation:** The project-scoped `ProtectedResource` and role change land in
  the same change as the mirror, not as a follow-up.
- **Risk:** A future service with a very large entitled-Location footprint
  drives per-project object counts up faster than expected.
  **Mitigation:** Object count is usage-bound, not catalog-bound (see [Object
  count](#object-count)); revisit if a specific service's adoption pattern makes
  this a real concern.

## Design Details

Illustrative, not exact — field names and shapes are a technical design question
for the follow-up implementation, consistent with how
[locations-platform-primitive.md](./locations-platform-primitive.md) treats its
own examples.

**The projected `Location`, service-agnostic, one per (project, Location):**

```yaml
# Project: acme-corp-project
apiVersion: locations.miloapis.com/v1alpha1
kind: Location
metadata:
  name: us-east-1
  labels:
    app.kubernetes.io/managed-by: services.miloapis.com
    networking.datumapis.com/location: us-east-1
    networking.datumapis.com/class: datum-managed
    # services.miloapis.com/service-name is dropped — no longer meaningful
    # once one projection can represent more than one entitled service.
spec:
  locationClassRef:
    name: datum-managed
  topology:
    topology.datum.net/city-code: NYC
status:
  conditions:
    # The platform Location's own conditions, mirrored by #85. Service-agnostic
    # by construction, which is what makes them safe to keep here.
    - type: Ready
      status: "True"
```

**`ServiceAvailability`, mirrored per (service, Location) into every project
holding an active entitlement for that service:**

```yaml
# Project: acme-corp-project — mirrored from the platform copy because
# Acme holds an Active entitlement for Compute.
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceAvailability
metadata:
  name: compute-miloapis-com--us-east-1
spec:
  serviceRef:
    name: compute-miloapis-com
  locationRef:
    name: us-east-1
status:
  conditions:
    - type: Available
      status: "True"
      reason: ServiceOperational
---
# No object-storage mirror exists at us-east-1 in acme-corp-project, because
# either Object Storage hasn't confirmed availability there yet, or Acme has
# no Object Storage entitlement in the first place — both cases look identical
# from inside the project: the record is simply absent.
```

The `<service>--<location>` name follows the convention in
`config/samples/services_v1alpha1_serviceavailability.yaml`; it is a convention,
not something the API enforces.

## Production Readiness Review Questionnaire

Deferred, consistent with
[locations-platform-primitive.md](./locations-platform-primitive.md#production-readiness-review-questionnaire).
This document stops short of a fully implementable technical design; the PRR
questionnaire belongs with the follow-up implementation once the reconcile-scope
and IAM mechanics are settled.

## Open Questions

- **What does `Available` mean on a service-agnostic `Location` projection?**
  Today `evaluateGates` writes a single per-service verdict there. Once the
  projection represents every entitled service, that condition either becomes an
  aggregate ("at least one entitled service is available here") or is dropped in
  favor of the platform conditions #85 mirrors plus the per-service records.
  Retaining it as an aggregate is the conservative choice, because existing
  readers may key off it, but the reader set is not fully enumerated (see
  [LocationBinding's retirement](#locationbindings-retirement)). Blocking for
  implementation.
- **Is the mirrored `ServiceAvailability` a literal copy of the platform
  object's spec/status, or a per-project recomputed verdict?** The platform
  object's `Available` condition does not itself fold in gate 1 (does the
  entitled service's published configuration support this Location's class);
  that is evaluated only inside `LocationBindingReconciler`. A straight copy
  therefore asserts something narrower than the projection's current
  `Available`. Non-blocking — either shape satisfies this document's product
  requirements, but it changes what the mirror asserts.
- **Should the multi-owner `ownerReferences` alternative be revisited** if
  project-wide recompute proves too expensive at scale? Non-blocking; noted in
  [Ownership and garbage collection](#ownership-and-garbage-collection).
- **Where does the mirror's reconcile live?** It shares every input with
  `LocationBindingReconciler` (root `ServiceAvailability` list, per-project
  Active entitlements) and could reasonably be a second writer inside the same
  reconcile pass or its own controller. Non-blocking.

## Implementation History

- 2026-08-29: Initial draft, scoping the two-entitlement projection failure and
  the lack of per-project `ServiceAvailability` visibility.

## Drawbacks

- Removing Kubernetes-native controller ownership means a projection's garbage
  collection is no longer visible in its `ownerReferences` — an operator
  debugging why an object persists or disappears has to reason about the
  reconciler's desired-state computation instead of reading owner references.
- Two objects instead of one means a consumer has to look in two places to get
  the full picture for a given service and Location, rather than reading one
  flag.
- The mirror adds a second, project-scoped `ProtectedResource` for a type that
  already has a platform-scoped one, a small but real increase in IAM surface to
  keep in sync.

## Alternatives

- **Keep one object, add a per-service map field to the `Location` projection
  instead of a separate mirrored type.** Rejected — it re-couples the two
  questions this document separates, and the projection is the same kind the
  locations service declares, so a services-specific field on it is not this
  repo's to add. A map keyed by service name also gets no independent IAM
  scoping or lifecycle.
- **Reference-counted garbage collection via multiple non-controller
  `ownerReferences`.** Considered; deferred rather than rejected — see
  [Ownership and garbage collection](#ownership-and-garbage-collection).
- **Don't mirror `ServiceAvailability`; have consumers read it from the root key
  space with narrowly scoped IAM.** Rejected — it breaks the isolation model
  every other project-visible resource here relies on, and scoping root IAM per
  consumer per service is a materially harder access-control problem than
  mirroring already solves elsewhere.

## References

- [Locations as Platform Primitives for Service
  Consumers](./locations-platform-primitive.md) — the three-gate model this
  document extends, and the `LocationBinding` reader table.
- [service-catalog#85](https://github.com/milo-os/service-catalog/pull/85) —
  mirrors platform `Location` status conditions onto the `Location` projection;
  in flight against the same reconciler, sequenced first.
- [infra#4299](https://github.com/datum-cloud/infra/pull/4299) — points staging
  at the locations service.
