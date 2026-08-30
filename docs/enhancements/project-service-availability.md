---
id: project-service-availability
title: Seeing Which Services Work Where, From Inside a Project
status: draft
created: 2026-08-29
updated: 2026-08-30
author: Scot Wells
---

# Seeing Which Services Work Where, From Inside a Project

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [What a consumer sees](#what-a-consumer-sees)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Open Questions](#open-questions)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [References](#references)

## Summary

A project using one service can discover, from its own control plane, which
locations that service works at. A project using **two** services cannot. The
second service's locations never appear, and never will.

This proposes separating two questions that are currently answered by one
object: which locations a project can use at all, and which of the project's
services actually work at each of them. The first stops being tied to a single
service. The second becomes readable from inside the project, which it is not
today.

This extends [Locations as Platform Primitives for Service
Consumers](./locations-platform-primitive.md), which defined the availability
model these answers come from. It does not revisit that model. It scopes how a
project sees into it.

## Motivation

A project's control plane holds a `Location` for each place the project can use,
so a consumer can answer "where can I deploy this?" without leaving their
project. That record is produced once per service the project is entitled to.
That was fine when a project used one service.

**A second service fails permanently.** Each service claims sole ownership of
the location records it produces. The first service to run takes them. Every
other service in that project is refused, on every retry, forever. It is not a
race that settles; it is decided by identity and stays decided.

Nothing in production exercises this today, because projects hold a single
entitlement. It fires the first time any project holds a second one, which is a
product goal.

**The record cannot express the answer a customer needs.** It carries one
availability flag covering three separate facts: whether the service supports
that kind of location, whether the location itself is healthy, and whether the
service has confirmed it is actually running there. Two of those are per-service.
One flag on one per-location record cannot say "Compute is available in Dallas,
but the AI gateway is not." The platform models that distinction. A project
cannot see it.

**And the per-service answer is not visible from a project at all.** The record
of "this service is confirmed live at this location" lives in the platform's own
key space. A project member has no permission to read it and no copy of it. Even
without the failure above, there is nothing to look at.

### Goals

- Any number of services in the same project can offer locations without one
  displacing another.
- A project can see, per service it uses, which of its locations that service
  actually works at, rather than one aggregate signal.
- Discovery stays inside the project's own control plane, like every other
  consumer-facing resource.
- A service's availability is visible only to projects actually using it.

### Non-Goals

- Revisiting the availability model or the `Location` primitive themselves, both
  defined in
  [locations-platform-primitive.md](./locations-platform-primitive.md).
- A catalog for browsing availability of services a project does *not* use. See
  [Leakage](#leakage).
- Retiring `LocationBinding`, which depends on readers outside this repo.
- A migration plan for `edge.datum.net` or any other pre-existing surface.

## Proposal

Split one object into two.

| Question | Today | Proposed |
|---|---|---|
| Which places can this project use at all? | One record per service, per location. Breaks past one service. | One record per location, independent of service. |
| Which of the project's services work at each place? | Not visible from inside a project. | One record per service and location, for each service the project uses. |

A project's `Location` becomes what its name already implies: a place the
project can use. It exists because some service the project holds reaches it,
and carries only facts true of the place itself, not of any one service.

Availability becomes its own record, one per service and location, present in a
project only for services that project actually uses. Where a service has not
shipped, there is no record. The absence is the answer.

### User Stories

#### A project using two services can see both

Acme uses Compute and Object Storage. Today only whichever service ran first
gets any locations at all; the other silently gets none, forever. Under this
proposal both appear, and Acme can tell, per location, which of the two is
confirmed running there.

#### A service is live somewhere another isn't

At `us-east-1` Compute is live and Object Storage has not shipped yet. From
inside Acme's project, `us-east-1` shows as a usable place, with an availability
record for Compute and none for Object Storage. Acme learns this without
platform access and without a support ticket. Today they cannot learn it at all.

#### A service Acme never bought stays invisible

Acme does not use the AI gateway. Nothing about where the AI gateway runs
appears in Acme's project. The records are never created there, rather than
created and hidden.

### What a consumer sees

Two locations, one service live at both, a second service live at only one:

```
$ datumctl get locations
NAME           CLASS           CITY
us-east-1      datum-managed   IAD
us-central-1   datum-managed   DFW

$ datumctl get serviceavailabilities
NAME                             SERVICE           LOCATION       AVAILABLE
compute--us-east-1               compute           us-east-1      True
compute--us-central-1            compute           us-central-1   True
object-storage--us-east-1        object-storage    us-east-1      True
```

Object Storage is absent at `us-central-1`. That is the signal, and it reads the
same whether the service has not shipped there or the project does not use that
service at all.

Command and column names are illustrative.

### Risks and Mitigations

- **Risk:** A project's records are no longer cleaned up by the platform's own
  ownership rules, but by the reconciler working out what should exist.
  **Mitigation:** This is already how fan-out sets elsewhere in this service are
  maintained. What changes is the scope of the calculation, not the mechanism.
  It does mean cleanup depends on the controller running; see
  [Drawbacks](#drawbacks).
- **Risk:** Migration leaves behind a stale ownership marker, so removing one
  service deletes locations another service still needs.
  **Mitigation:** Ownership markers must be cleared explicitly during migration,
  covered by a test that removes one service and asserts the locations survive.
  This is the one part of migration that is not free. See
  [Migration](#migration).
- **Risk:** The availability records ship without the permissions to read them,
  and nothing errors. The records simply exist with no reader.
  **Mitigation:** The permission changes land with the records, not after.
- **Risk:** Recalculating a whole project's records on each pass costs more than
  today's per-service pass.
  **Mitigation:** Bounded by the number of services one project uses, which is
  small. The existing periodic refresh already assumes a full recalculation.

## Design Details

Light on mechanism by intent. Exact shapes belong to the implementation.

### Ownership and cleanup

This is the central decision.

Today each service claims sole ownership of the records it produces, and the
platform removes them when that service's entitlement goes away. Sole ownership
is exactly what makes a second service fail, so it has to go. Once a location
record is shared, no single service is the right owner: it should exist as long
as *any* service in the project still needs it.

The proposal is to calculate at the level of the project rather than one
service. A change to any service still triggers the work, but the work considers
every service the project actively uses, decides the full set of records that
should exist, writes them, and removes anything left over. Removing the last
service in a project empties that set and cleans up everything.

An alternative is to let each service hold a non-exclusive claim and have the
platform delete a record once no claims remain, which gives reference-counted
cleanup for free. It is set aside rather than rejected: it would add a second
cleanup mechanism alongside the one this service already uses everywhere else.
Worth revisiting if project-wide recalculation proves expensive.

Project deletion needs nothing. The project's whole control plane goes away with
it.

### Permissions

A project's `Location` needs no new work. The locations service already
registers it as a project-scoped resource with a viewer role.

The availability records are the gap. Today that type is registered only as a
platform resource, so a project member has no path to read it. Mirroring the
records without registering them as project-scoped would ship objects nobody
entitled to see them can read. The fix is a project-scoped registration and a
viewer role, either new or folded into the existing entitlement viewer.

### Leakage

Which services exist and where they have shipped is not necessarily something
every customer should be able to enumerate. Records are mirrored into a project
only for services that project actively uses, which is the restriction the
current path already applies for its single service. No cross-project or
aggregate surface is introduced.

### Object count

Per project: one record per location the project can reach, plus one per service
and location where that service is available. That is bound by what a project
uses, not by the size of the platform's catalog, which is the shape it already
has. No bounding needed at current scale. Revisit if the location catalog
reaches the hundreds and projects routinely hold many services.

### Migration

Records keep their names and identity. Existing ones are updated in place, not
recreated. Nothing is dual-written and consumers see no cutover.

The exception is the stale ownership marker described in
[Risks](#risks-and-mitigations), which must be cleared deliberately.

Sequencing:

1. [#85](https://github.com/milo-os/service-catalog/pull/85) first. It is
   additive, does not touch ownership, and changes the same area. Rebasing it
   onto an ownership rewrite is more work than the reverse.
2. The ownership fix alone. It is a pure bugfix for something already broken.
3. The availability records and their permissions, together. A project's
   location visibility does not depend on them.

### LocationBinding

`LocationBinding` is being retired, and this design adds nothing to it. It
inherits the ownership fix, which it needs, because it fails on a second service
the same way. When its last reader moves, it drops out of this design with no
other change.

The reader set is in flux, and
[locations-platform-primitive.md](./locations-platform-primitive.md#migration-off-locationbinding),
not this document, is the source of truth for it.

## Production Readiness Review Questionnaire

Deferred, consistent with
[locations-platform-primitive.md](./locations-platform-primitive.md#production-readiness-review-questionnaire).
This document stops short of an implementable technical design.

## Open Questions

- **What does the availability flag on a shared location record mean?** Today it
  carries one service's verdict. Once the record covers every service, it either
  becomes an aggregate, meaning at least one service works here, or it goes away
  in favour of the per-service records. Keeping it as an aggregate is
  conservative, because existing readers may depend on it, but the reader set is
  not fully known. **Blocking for implementation.**
- **Is a mirrored availability record a copy of the platform's, or recalculated
  per project?** The platform's own record does not account for whether the
  service supports that kind of location, so a straight copy asserts something
  narrower than today's flag. Either satisfies the product requirements here,
  but they assert different things. Non-blocking.
- **Should reference-counted cleanup be revisited** if project-wide
  recalculation proves expensive? Non-blocking.
- **Where does the mirroring work live?** It shares every input with the
  existing location work and could sit alongside it or stand alone.
  Non-blocking.

## Implementation History

- 2026-08-29: Initial draft, scoping the two-service failure and the lack of
  per-project availability visibility.
- 2026-08-30: Recast around product behaviour, with mechanism reduced to
  [Design Details](#design-details).

## Drawbacks

- Cleanup is no longer visible in the record itself. Someone debugging why an
  object persists has to reason about what the controller decided, rather than
  reading ownership off the object. It also means cleanup depends on the
  controller running to observe a change.
- A consumer reads two records rather than one flag to get the full picture for
  a service and a place.
- A second, project-scoped registration for a type that already has a
  platform-scoped one, which is a small increase in permission surface to keep
  in step.

## Alternatives

- **Keep one record and add a per-service section to it.** Rejected. It
  re-couples the two questions this separates, and the record is the same kind
  the locations service declares, so a services-specific field on it is not this
  repo's to add. It also gets no independent permissions or lifecycle.
- **Reference-counted cleanup.** Deferred rather than rejected. See
  [Ownership and cleanup](#ownership-and-cleanup).
- **Have consumers read the platform's records directly under narrow
  permissions.** Rejected. It breaks the isolation model every other
  project-visible resource relies on, and scoping platform permissions per
  consumer per service is a harder access-control problem than mirroring, which
  is already solved elsewhere.

## References

- [Locations as Platform Primitives for Service
  Consumers](./locations-platform-primitive.md)
- [#85](https://github.com/milo-os/service-catalog/pull/85), mirroring platform
  location status into a project
- [datum-cloud/infra#4299](https://github.com/datum-cloud/infra/pull/4299),
  pointing staging at the locations service
