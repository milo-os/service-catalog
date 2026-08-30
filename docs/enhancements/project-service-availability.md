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

A project that uses one service can list the locations that service works at. A
project that uses two services cannot. The second service's locations never
appear.

This document proposes splitting one record into two:

- A `Location`, naming a place the project can use.
- An availability record, naming a service that works at that place.

It extends [Locations as Platform Primitives for Service
Consumers](./locations-platform-primitive.md), which defines the availability
model. It does not change that model.

## Motivation

Each project's control plane holds a `Location` for every place the project can
use. A consumer lists those locations to decide where to deploy. Each service
the project uses produces its own copy.

Three problems follow.

**A second service fails permanently.** Each service claims sole ownership of
the locations it produces. The first service to run takes them. Every later
service is refused on every retry. The failure is deterministic, so it never
resolves.

Production does not hit this today, because each project uses one service. It
fires the first time a project uses two, which is a product goal.

**One flag cannot answer a per-service question.** Each `Location` carries a
single availability flag covering three facts:

1. The service supports that kind of location.
2. The location is healthy.
3. The service runs there.

Facts 1 and 3 differ per service. One flag on one location cannot say that
Compute works in Dallas but the AI gateway does not. The platform records that
distinction. A project cannot read it.

**Projects cannot read availability at all.** The record of where a service runs
lives in the platform's key space. A project member has no permission to read it
and no copy of it.

### Goals

- Let any number of services in a project publish locations.
- Let a consumer see which services work at each location.
- Keep discovery inside the project's control plane.
- Show a service's availability only to projects that use that service.

### Non-Goals

- Changing the availability model or the `Location` resource.
- Browsing availability for services a project does not use.
- Retiring `LocationBinding`, which depends on readers outside this repo.
- Migrating `edge.datum.net` or other existing surfaces.

## Proposal

One record answers two questions today, and answers both badly.

**Which places can this project use?** One record exists per service and
location, so the record fails past one service. Keep one record per location
instead.

**Which services work at each place?** The project cannot see this at all. Add
one record per service and location.

A `Location` names a place, not a service. It exists while any service in the
project reaches it. It carries only facts about the place.

An availability record names one service at one location. A project holds
records only for the services it uses. Where a service does not run, no record
exists.

### User Stories

**A project that uses two services sees both.** Acme uses Compute and Object
Storage. Today, whichever service runs first publishes locations and the other
publishes none. Under this proposal both publish, and Acme sees which service
works at each location.

**A consumer finds where a service runs.** Compute runs at `us-east-1`. Object
Storage does not. Acme reads both facts from its own project, without platform
access and without a support ticket.

**Unused services stay invisible.** Acme does not use the AI gateway. Acme's
project holds no AI gateway records. The records are never created, not created
and hidden.

### What a consumer sees

```
$ datumctl get locations
NAME           CLASS           CITY
us-east-1      datum-managed   IAD
us-central-1   datum-managed   DFW

$ datumctl get serviceavailabilities
NAME                        SERVICE          LOCATION       AVAILABLE
compute--us-east-1          compute          us-east-1      True
compute--us-central-1       compute          us-central-1   True
object-storage--us-east-1   object-storage   us-east-1      True
```

Object Storage has no record at `us-central-1`. A missing record means the
service does not run there, or the project does not use it. Both read the same.

Command and column names are illustrative.

### Risks and Mitigations

**The controller, not the platform, cleans up records.** Other fan-out sets in
this service already work this way. Cleanup now depends on the controller
running. See [Drawbacks](#drawbacks).

**A stale ownership marker survives migration.** Removing one service then
deletes locations another service needs. Clear the marker during migration. Test
that removing one service leaves the locations in place.

**Availability records ship without read permission, and nothing errors.** Ship
the permission change with the records.

**Recalculating a whole project costs more than recalculating one service.** The
count of services in a project is small. The periodic refresh already
recalculates in full.

## Design Details

This section states decisions, not implementation.

### Ownership and cleanup

Sole ownership causes the failure, so it has to go. A shared `Location` has no
single owner. It should exist while any service in the project needs it.

The controller therefore works per project, not per service. A change to one
service triggers the work. The work then:

1. Reads every service the project uses.
2. Decides which records should exist.
3. Writes them.
4. Deletes the rest.

Removing the last service empties the set and deletes everything. Deleting a
project removes its control plane, so project deletion needs no handling.

An alternative gives each service a non-exclusive claim and lets the platform
delete a record once no claims remain. That yields reference-counted cleanup at
no cost. It is deferred, not rejected: it adds a second cleanup mechanism
alongside the one this service already uses. Revisit it if per-project
recalculation proves expensive.

### Permissions

`Location` needs no change. The locations service already registers it as
project-scoped and ships a viewer role.

Availability records need work. The platform registers that type for platform
scope only, so a project member cannot read it. Mirroring records without a
project-scoped registration ships objects that nobody can read. Add a
project-scoped registration and a viewer role.

### Leakage

Which services exist, and where they run, is not public. A project holds records
only for services it uses. This design adds no cross-project surface.

### Object count

Per project, the count is one record per location the project reaches, plus one
per service and location where the service runs. Usage bounds the count, not the
catalog. No limit is needed now. Revisit if the catalog reaches hundreds of
locations and projects routinely use many services.

### Migration

Records keep their names. Existing records update in place. Consumers see no
cutover.

Clearing the stale ownership marker is the one step that is not free. See
[Risks](#risks-and-mitigations).

Ship in this order:

1. [#85](https://github.com/milo-os/service-catalog/pull/85). It is additive,
   leaves ownership alone, and touches the same code.
2. The ownership fix, alone. It fixes an existing bug.
3. The availability records and their permissions, together.

### LocationBinding

`LocationBinding` is being retired. This design adds nothing to it. It inherits
the ownership fix, which it needs, because it fails on a second service the same
way. When its last reader moves, it leaves this design with no other change.

For the current reader set, see
[locations-platform-primitive.md](./locations-platform-primitive.md#migration-off-locationbinding).

## Production Readiness Review Questionnaire

Deferred, as in
[locations-platform-primitive.md](./locations-platform-primitive.md#production-readiness-review-questionnaire).
This document stops short of an implementable design.

## Open Questions

- **What does the availability flag on a shared `Location` mean?** It carries
  one service's verdict today. Once the record covers every service, it either
  aggregates, meaning at least one service works here, or it goes away in favour
  of the per-service records. Aggregating is the conservative choice, because
  readers may depend on the flag, but the reader set is unknown. **Blocking.**
- **Does an availability record copy the platform's record, or recalculate per
  project?** The platform's record ignores whether the service supports that
  kind of location, so a copy asserts less than today's flag. Both satisfy the
  goals above. Non-blocking.
- **Should reference-counted cleanup be revisited** if per-project
  recalculation proves expensive? Non-blocking.
- **Where does the mirroring run?** It shares every input with the existing
  location work. Non-blocking.

## Implementation History

- 2026-08-29: Initial draft.
- 2026-08-30: Recast around consumer behaviour. Mechanism moved to
  [Design Details](#design-details).

## Drawbacks

- Ownership no longer appears on the record. To learn why a record persists, a
  reader has to know what the controller decided. Cleanup also depends on the
  controller running.
- A consumer reads two records instead of one flag.
- A second registration for a type that already has one adds permission surface
  to keep in step.

## Alternatives

- **Add a per-service section to `Location`.** Rejected. It recouples the two
  questions, adds a services-specific field to a resource this repo does not
  own, and gets no separate permissions or lifecycle.
- **Reference-counted cleanup.** Deferred. See
  [Ownership and cleanup](#ownership-and-cleanup).
- **Let consumers read platform records under narrow permissions.** Rejected. It
  breaks the isolation model that every other project-visible resource relies
  on, and per-consumer, per-service platform permissions are harder than
  mirroring.

## References

- [Locations as Platform Primitives for Service
  Consumers](./locations-platform-primitive.md)
- [#85](https://github.com/milo-os/service-catalog/pull/85), mirroring location
  status into a project
- [datum-cloud/infra#4299](https://github.com/datum-cloud/infra/pull/4299),
  pointing staging at the locations service
