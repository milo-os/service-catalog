# Entitlement registration should enroll the registrant in a CRM contact group

**Status:** Proposed
**Scope:** Enrollment logic lives entirely in service catalog. It writes to
Milo's existing `Contact`/`ContactGroup` notification API as a client;
nothing changes on Milo's side.

Source issue: datum-cloud/cloud-portal#1497.

## Summary

When a customer registers for a service (for example, Compute), internal
staff want that person's CRM contact automatically added to the matching
Contact Group (for example, `compute-testers`), so they don't have to
maintain that list by hand. This keeps internal contact lists in sync with
real product usage instead of relying on someone remembering to update them.

Service catalog owns this end to end: when a customer's registration for a
service goes active, and that service has been set up with a Contact Group,
service catalog adds the customer's contact to that group, creating the
group automatically the first time it's needed.

## Why this lives in service catalog

An earlier version of this proposal had Milo's contact system watch service
registrations directly. That was rejected: Milo's core shouldn't need to
understand what a service or an entitlement is, the same way billing
doesn't need to understand what a service is to meter and charge against
one. Service catalog already understands services and entitlements, so it
drives this itself, writing to Milo's contact API the same way any other
client would.

## Goals

- A customer registering for a service that's opted into CRM tracking gets
  added to the right Contact Group automatically, without a human
  remembering to do it.
- The mapping from a service to its Contact Group is something an operator
  sets up once per service.
- Milo's contact system stays unaware of services and entitlements.

## Non-Goals

- Automatically removing someone from a Contact Group when their
  registration is revoked or rejected — that stays a manual/opt-out action
  in this iteration.
- Letting customers set contact info per project/org to drive group
  membership (raised in the issue thread) — a materially different feature.
- Any change to how Contact Groups are used or configured outside of this
  enrollment flow.

## How it works

An operator sets up a service to opt into this behavior by linking it to a
Contact Group. Services that don't set this up stay unaffected.

When a customer registers for that service and the registration goes
active, service catalog adds their CRM contact to the linked group. If the
group doesn't exist yet, service catalog creates it automatically rather
than requiring an operator to create it first.

- If the customer doesn't have a CRM contact yet, the enrollment isn't
  lost — it completes as soon as their contact record shows up.
- If someone has already opted out of a group, that opt-out is respected;
  they won't be re-added.
- If a customer registers for the same service more than once (for example,
  across two projects), they only end up in the group once.
- If an operator links a Contact Group to a service after customers have
  already registered, those existing registrations are picked up and
  enrolled too, not just new ones going forward.

## Design

This stays entirely within service catalog; Milo's contact system is a
passive API it writes to, not a participant in the decision-making.

- Service catalog already tracks when a customer's registration for a
  service goes active. A new piece of logic watches for that, and for each
  newly-active registration, checks whether the service it's for has a
  Contact Group linked.
- If so, it resolves the customer's CRM contact and adds them to that group
  by writing directly to Milo's existing contact API — the same API a human
  operator or any other client would use. No new integration point is added
  on Milo's side, and Milo's own enrollment mechanism (used for other,
  non-service-specific cases) is untouched.
- Service registrations today don't record who requested them. Closing that
  gap is a prerequisite: service catalog needs to capture the requesting
  customer's identity at registration time so it can look up their contact
  later. This is a small, self-contained addition to how registrations are
  created.
- Service catalog creates the linked Contact Group automatically the first
  time it's needed, using sensible defaults. An operator can adjust the
  group's settings (for example, visibility, or which external systems it
  syncs to) afterward in staff-portal, the same way they manage any other
  Contact Group today.

## Risks and mitigations

| Risk | Impact | Mitigation |
| --- | --- | --- |
| No automatic removal on revocation | A revoked customer stays listed as a tester until someone manually removes them | Explicit, called-out scope cut; confirm acceptable with support/success/sales before shipping |
| Auto-created Contact Group has no sensible default for visibility/sync settings | A newly created group may not sync anywhere until an operator notices and configures it | Operator reviews and adjusts auto-created groups' settings after the fact; revisit if this proves to be a recurring gap |
| Dependency-origin registrations counted the same as direct ones | CRM lists may include people who didn't directly opt in | Flagged as an open product question, not silently decided |

## Open questions

- Should a customer who received a service automatically (as a side effect
  of registering for something else it depends on) be enrolled the same as
  one who registered directly? This proposal currently treats them the
  same, and the implementation does too — a dependency-origin entitlement
  carries its parent's requester forward. Still open as a *product*
  question (not an implementation one): revisit if this proves to be a
  recurring gap.
- Confirm with stakeholders that no automatic membership removal on
  revocation is acceptable for this first iteration. Still open — this is a
  support/success/sales call, not an engineering one, and the implementation
  performs no teardown either way.
- What defaults should an auto-created Contact Group start with (visibility,
  external sync destinations)? Resolved for the first iteration: public
  visibility (so an opt-out is always honored — private would reject it
  outright) and no provider sync destinations (Milo's ContactGroup.spec.providers
  is add-only with immutable IDs, so guessing one would be unrecoverable). An
  operator attaches the right provider afterward in staff-portal, same as any
  other auto-created group.

## Implementation

Shipped in `service-catalog` across four phases; each phase's commit message
has the detail, this is the map:

- **`ServiceEntitlement.spec.requestedBy`** (`api/v1alpha1/serviceentitlement_types.go`)
  — stamped by a create-only mutating webhook
  (`internal/webhook/v1alpha1/serviceentitlement_webhook.go`) from the
  admission caller's identity. `UserInfo.UID` is used as the join key because
  for a Milo `User` it *is* the `User.metadata.name` — the same value Milo's
  own `Contact.spec.subject.name` carries. Immutable after create. A
  dependency entitlement inherits its parent's value
  (`ensureDependencies` in `internal/controller/serviceentitlement_controller.go`).
- **`Service.spec.contactEnrollment`** (`api/v1alpha1/service_types.go`) — the
  operator-set link to a `ContactGroup`. Repointing an already-set
  `contactGroupRef` is rejected by validation; adding or removing the whole
  block is unrestricted.
- **`internal/contactenrollment/`** — the client-facing logic: resolve a
  `Contact` by subject name (falling back to email), get-or-create the
  `ContactGroup`, and enroll with a deterministic membership name so the same
  requester/group pair always collapses to one membership regardless of how
  many entitlements produced it.
- **`ContactEnrollmentReconciler`** (`internal/controller/contact_enrollment_controller.go`)
  — a separate controller from `ServiceEntitlementReconciler` by design: CRM
  enrollment is best-effort and must never block dependency enrollment or
  quota grants for the same entitlement. Reports its own
  `ContactEnrolled` condition, gated on `ServicesOperator.contactEnrollment`
  being configured (nil = off, matching the `webhookServer` /
  `consumerScopedProjection` pointer-gate convention already in
  `internal/config`).
- **Tests**: unit and fake-client controller tests throughout; a real
  end-to-end suite at `test/e2e-milo/contact-enrollment/`, which is the only
  coverage that exercises real admission-time `requestedBy` stamping and a
  real `ContactGroup`/`ContactGroupMembership`/`ContactGroupMembershipRemoval`
  round trip against Milo's own API — including the opt-out-is-honored path.

## Alternatives considered

- **Drive this from Milo instead of service catalog**: rejected because it
  would require Milo's core contact system to understand what a service and
  an entitlement are, breaking the separation Milo currently has (and that
  billing already follows) between generic platform concepts and
  service-specific ones.
- **Require an operator to pre-create the Contact Group** rather than
  auto-creating it: considered and initially recommended, but revised in
  favor of removing that manual step; an operator can still adjust the
  auto-created group's settings afterward.
