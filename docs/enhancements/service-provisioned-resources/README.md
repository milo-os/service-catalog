---
status: implementable
stage: alpha
latest-milestone: "v0.x"
---

# Service-Provisioned Resources

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [What a provider declares](#what-a-provider-declares)
  - [What a consumer receives](#what-a-consumer-receives)
  - [User Stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Why an embedded object](#why-an-embedded-object)
  - [Security model](#security-model)
  - [The authorization gap](#the-authorization-gap)
  - [Lifecycle](#lifecycle)
  - [Failure legibility](#failure-legibility)
  - [Relationship to unconditional per-project seeding](#relationship-to-unconditional-per-project-seeding)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Open Questions](#open-questions)

## Summary

Turning on a service should be the whole action. Today it is not: a project that enables networking still cannot allocate an address, because nothing puts an address class in its control plane. Every consumer of the API hand-writes classes as test fixtures, and no real project has any. The service that owns the concept has no way to say "a project using me needs this."

A provider can now declare, in the `ServiceConfiguration` it already authors, what a project should be given when it enables the service. It writes out the objects themselves. The platform installs them into every project holding an Active entitlement and removes them when the entitlement stops being Active. The provider declares once; every project that enables the service receives them, after any approval, with a consumer-visible statement of whether they arrived.

The declaration is a list of objects, not a template. There are no variables, no per-project values, and no field paths: every entitled project receives the same object under the same name. What bounds the capability is therefore not the content but the destination — objects reach only projects that asked for the service, and only after approval.

## Motivation

Three problems converge here.

**The concrete one.** IP classes do not exist in real projects. The platform owns the ranges; a project needs to name a class to allocate from one. Nothing puts anything in a project because of a service, so classes are only ever seen in fixtures. Compute inherits the same gap through its dependency on networking.

**The structural one.** `ServiceEntitlement` reconciliation has two fan-outs — dependency enrollment and quota grants — plus a separate reconciler that projects locations. There is no extension point, so each new "when a project enables this service, it also needs X" has meant a new hand-written controller with its own naming, labelling, pruning, and mostly absent status reporting. There is no reason to expect the list of Xs to stop growing.

**The trust one.** This is a capability for one party to cause writes into another party's control plane. That is already true of quota grants and location bindings, but they are bounded tightly enough that the trust is easy to miss. Generalizing the capability without generalizing the bounds is the real risk, and most of the design below is about that.

There is also a decision already on record. [Consumer project engagement](../consumer-project-engagement.md) states the intended end state: the catalog should be the authoritative declaration of the resource types a service manages in a consumer project, so discovery, approval-time governance, access scoping, and cleanup all derive from one published fact. Its `ManagedResources` operator config is explicitly an interim stand-in for that declaration. This is the catalog half of that plan.

### Goals

- Let a provider declare, in the artifact it already authors, what a consumer project needs in order to use its service.
- Deliver those resources only to projects that enabled the service, and only after any provider approval.
- Remove them when the service stops being enabled, without every provider writing its own teardown.
- Tell a consumer, in terms of the service that owed them, when resources did not arrive.
- Keep the declaration readable as what it is: the object a project will hold, written out.

### Non-Goals

- Not a package manager. No dependency resolution among installed resources, no ordering graph, no rollback beyond convergence.
- Not a renderer. No templating, no expression language, no substitution of any kind.
- Not a way to change a project plane's API surface. What a project serves is resolved at the discovery layer.
- Not consumer-configurable. A consumer supplies no values; enabling the service is the only input.

## Proposal

### What a provider declares

The networking team keeps its real ranges, and the classes describing them, in one project it owns, managed in version control. That does not change.

To make those classes reachable from consumer projects, the team adds a `spec.provisioning` block to the `ServiceConfiguration` it already publishes — the same document where it declares its metrics, its quota limits, and the location classes it runs on. Each entry is a name and a list of objects:

```yaml
spec:
  provisioning:
    resources:
      - name: address-classes
        objects:
          - apiVersion: ipam.miloapis.com/v1alpha1
            kind: IPClass
            metadata:
              name: tenant-endpoint-ipv6
            spec:
              source:
                project: platform-networking
                name: tenant-endpoint-ipv6
```

The object is a reference to the platform's own class, so nothing is copied and nothing drifts. That is IPAM's design, not the platform's rule: a class with `spec.source` states no policy of its own, and the reference resolves through to the numbering the networking team controls. A provider whose API has no such concept declares whatever its API does take.

Offering another class means adding another object and republishing. Editing an object is how a provider changes what consumers hold; removing one withdraws it.

Compute declares networking as a dependency, as it already does. It inherits the classes without declaring anything, because a dependency entitlement is a real entitlement and provisions like any other.

The provider never learns how a project control plane is addressed, what credential writes into it, or how many consumers exist.

### What a consumer receives

A project admin enables networking — one `ServiceEntitlement`, or one `datumctl services enable`. If networking is `GatedByProvider`, nothing is installed while the request sits `PendingApproval`. Installation follows approval; it does not anticipate it.

Once the entitlement is Active, listing IP classes in the project returns the platform's classes, under the names the provider chose. The project did not create them and cannot be missing them. Each carries the service that installed it, the entitlement it arrived under, and the declaration that produced it, as labels — which is where the provenance question is answered, because the name is now the provider's to pick.

If something did not arrive, the entitlement says so, in the project's own control plane, naming the service that owed it:

```
Provisioned  False  PartiallyProvisioned
  2 of 3 declared resources installed.
  networking.datumapis.com could not install IPClass "public-unicast" because
  this project's control plane does not serve that kind.
```

The consumer does not discover the problem later as an unrelated component reporting a symptom. That is not hypothetical — when location projection stopped in staging, the visible signal was compute reporting that no locations were registered, weeks after the fact.

Disabling the service removes what was installed.

### User Stories

1. **Classes reach a real project.** A project enables networking and can immediately allocate an address from a platform-managed class, without anyone hand-creating a class in that project and without the project being able to invent its own numbering.
2. **A provider changes what it offers.** The networking team edits the declaration. Every already-entitled project converges: the new object arrives, the one no longer declared is removed.
3. **Inheriting through a dependency.** A project enables compute. Compute depends on networking, a dependency entitlement is created, and once Active it provisions networking's resources into the same project. The consumer never explicitly enabled networking.
4. **A platform operator answers "why is this object here."** An object in a consumer project carries the service that installed it, the entitlement it was installed under, and the declaration that produced it — all answerable from the object alone.

### Notes/Constraints/Caveats

- **The active configuration is a live document.** `ServiceConfiguration` is mutable and unversioned, and *additions are always allowed*, so a provider can add a provisioning declaration to an already-Published configuration and have it reach every existing consumer. That is the status quo for quota, and it matters more once the declaration is more expressive. Nothing inside a declaration is frozen on publish: editing the object is the supported way to update what consumers hold, and pruning is scoped by the declaration's name, so a rename or a kind change converges rather than stranding the old object.
- **Names can collide.** The provider chooses the installed name, so two providers can declare objects of one kind under one name in the same project. The second to reconcile wins, and neither declaration is refused. The platform used to derive the name and prevent this; it no longer does.
- **Configuration selection is inconsistent across fan-outs.** Provisioning takes the most recently created Published configuration, matching the location reconciler; the quota fan-out takes the first Published one in list order. One rule should win.
- **The operator engages every Ready project**, not only entitled ones. Narrowing that is the engagement enhancement's work.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| The mechanism becomes a general "write anything anywhere" primitive | It largely is one, within a shape: any object with a dotted API group, no namespace, and no platform-owned metadata, into projects that enabled the service. See [Security model](#security-model) for what that does and does not exclude |
| A provider installs an object naming another provider's resources | Not prevented. The source-ownership check went with the projection; see [Security model](#security-model) |
| Provisioning launders authorization past the target API's own checks | Not solved in this version; reported per resource on the entitlement rather than left implicit. See [The authorization gap](#the-authorization-gap) |
| A provider change silently reaches every consumer at once | Per-resource status on every entitlement; a rollout gate is an open question |
| One declaration fans out unboundedly | Caps on objects per declaration and declarations per configuration; exceeding one is refused and reported, never truncated |
| Teardown strands a project mid-way | Installed objects are owner-referenced to the cluster-scoped entitlement, so deleting it garbage-collects them; label-scoped pruning covers withdrawn declarations |
| Disabling a service removes resources still in use | The owning API, not service-catalog, refuses to leave dangling state — an open question |
| Cluster-scoped installed objects survive project deletion | Known gap in the project purger; called out rather than assumed away |

## Design Details

### Why an embedded object

The earlier design had the provider name a source project, a kind, and a label selector, and the platform built one reference object per matched source object. The provider stated where the reference went and under which two keys; it could not state a value.

That bought a real property — the content was derived from objects created under ordinary authorization — and cost more than it bought. It only worked for APIs shaped like a cross-project reference, it needed a path-and-keys mini-language that is a template engine with one template, and the platform had to hold a model of another team's API to write into it. It kept getting that wrong: the entry named the wrong API group for two months without anything catching it.

Embedding removes all of that. The provider writes what it wants installed; the platform stores it and applies it, and the declaration is reviewable as the thing it produces. For IPAM the embedded object is still a reference, so "the consumer holds a pointer, not a copy" survives as IPAM's property to enforce, which is where it belongs: IPAM rejects a class carrying both a source and a policy, freezes the reference once created, and refuses chains. What the platform still decides is everything around the object — the labels, the owner reference, and the destination — and a declaration that tries to state them is refused.

### Security model

The services operator authenticates to Milo with a client certificate whose organization is `system:masters`, and reaches every project control plane by copying that config and rewriting the URL path. There is no per-project credential, no per-provider credential, and no impersonation. The operator's ClusterRole holds no grants for the kinds it installs, and could not usefully: a `system:masters` subject is not constrained by RBAC, and the kinds are named by providers at runtime. A per-kind grant would enforce nothing while implying a ceiling.

Two consequences are load-bearing. Anything a provider can express in a `ServiceConfiguration` is executed by an omnipotent identity in the consumer's control plane. And the bound therefore cannot be delegated to RBAC — a design that says "the ClusterRole is the ceiling" describes a control that is not in effect. Narrowing the operator's identity is worth doing on its own merits, but it is a Milo-side change and this design has to be correct without it.

**What still bounds the write.** Three things, and they are about the destination and the shape, not the content:

- **The pull direction.** A service installs only into projects that created an entitlement for it, and only once that entitlement is Active — after approval, for a `GatedByProvider` service. A provider declares; nothing happens until a consumer asks. A provider cannot enumerate or target projects.
- **The shape of an object.** The API group must be a dotted domain, so the core group cannot be named and no `Secret`, `ConfigMap`, or `ServiceAccount` is reachable. The version must be a served API version. A name is required and a `generateName` is refused, because the platform writes idempotently. A namespace is refused, because provisioning does not create namespaces. Owner references and finalizers are refused, because the platform sets the owner reference that makes deleting the entitlement reclaim the object, and a finalizer would defeat teardown. `status` is refused. Objects are capped per declaration, declarations per configuration, and bytes per object.
- **The owning API's own judgement.** Whether an object is acceptable is decided by the API that owns the kind, when it accepts or refuses the write. IPAM's rules on `spec.source` are IPAM's, not a table here. A platform table restating them would be a second, worse copy of a rule it does not own.

That is enforced twice, and the repetition is not redundant. The `ServiceConfiguration` schema marks each entry as an embedded resource, so the API server refuses an object with no `apiVersion`, no `kind`, or malformed metadata with no webhook in the picture. Admission adds the content rules above, so a provider gets a synchronous error rather than discovering the refusal in a consumer's status later. And the controller re-checks every object before it writes, because a document admitted under an earlier schema, or while the webhook was absent, stays in etcd. A declaration carrying one forbidden object is refused whole, not in part.

**What is no longer enforced, stated plainly.** A projection could only read out of the project the declaring service was published from, and that check was the answer to "can a provider reach another provider's resources." With the object embedded there is no source project for the platform to read, so there is nothing to check and the check is gone. Nothing now stops a provider from declaring an object that *names* another provider's project — an `IPClass` whose `spec.source.project` is someone else's platform project is admitted and installed. Whether that reference resolves is the owning API's decision, and its authorization check runs against the operator, which passes it (see below). So in this version a provider can cause consumer projects to hold references to resources it does not own. We accept that: the platform is the only provider today, and the fix is the per-service write identity that also closes the authorization gap, not a new field to compare.

The residual on the shape rule is worth stating too. Granting access normally takes a subject and a permission, and both `PolicyBinding` and RBAC objects are refused by group or by their own required fields. But an embedded object is arbitrary within the shape, so a future API that confers authority through a kind the shape admits would be installable. The bound then is the owning API's own check — which is the authorization gap, and is why closing it matters more than a deny list would.

### The authorization gap

This is the honest limit of the first version.

Where the target API authorizes a reference itself, it does so against whoever creates the object. IPAM does this well: creating a class that names another project's class triggers a subject access review for `use` on the source class, with the caller's scope rewritten to the source project, and it fails closed. But the caller here is the operator, whose identity passes that check trivially. The reference exists and works on the strength of the installer's authority rather than the consumer's — the project never held `use` on the platform's class. Revoking access later does not undo it, because the check runs at create, not at use.

Nothing in this version closes that. The gap is reported instead of hidden: each project's entitlement status carries `authorizationEstablished` per resource, with a consumer-facing explanation of what it means. It is false unconditionally, because nothing establishes a consumer grant for any kind — reporting it per kind would describe the target API's rigour rather than what the platform did.

Closing it requires the platform to establish a real grant — a platform-authored IAM binding, in a separate typed fan-out where the provider names only its own resources and the platform chooses the subject, the scope, and the verb — so the target API's own check would independently accept the write. That is separate work, and it is a prerequisite for the motivating case being correct rather than merely working.

### Lifecycle

| Event | Behavior |
| --- | --- |
| Provider publishes a changed declaration | The fan-out watches `ServiceConfiguration` on the root cluster; declared objects are applied, withdrawn ones pruned |
| Consumer edits or deletes an installed object | Restored on the next reconcile. The apply is server-side and forced, so every field the declaration states is the platform's |
| Entitlement leaves Active — rejected, revoked, deleting | Everything installed for it is torn down. Not-Active and deleting are treated identically |
| Entitlement deleted | Installed objects are owner-referenced to the cluster-scoped entitlement and garbage-collected |
| Declaration withdrawn | Pruned, scoped by service, entitlement, and declaration name, so one service's fan-out cannot remove another's objects. The kinds come from the entitlement's own ledger, which records what this project received, because a withdrawn declaration no longer says what it installed |
| Object renamed or re-kinded in a retained declaration | The object under the old identity is removed, for the same reason and from the same record |
| Provisioning fails partway | Applied resources stay applied; the failure is retried and reported per resource |
| Target kind not served in the project plane | Reported as unprovisionable, not skipped |
| Project deleted | **Not reliably.** The project purger omits cluster-scoped resources, and both location bindings and IP classes are cluster-scoped. A pre-existing Milo gap that provisioning inherits |

Two of these are decisions rather than mechanisms. **Rollout** is immediate: a provider's edit reaches every consumer as soon as it converges, with no canary and no per-consumer pinning. That is the status quo for quota and locations, but the blast radius grows with the expressiveness of the declaration. **In-use resources at teardown** are not something service-catalog can reason about without learning per-service semantics; the division taken here is that service-catalog issues the delete and the owning API refuses to leave dangling state, surfacing as a stuck teardown with an honest message rather than silent corruption.

### Failure legibility

A project that did not get its resources says so, in its own control plane, in terms of the service that owed them.

`ServiceEntitlement` gains a `Provisioned` condition alongside `Ready`, and a per-resource ledger in status: for each declaration, the kinds it installed, how many objects, its state — installed, failed and retrying, or unprovisionable — and, when it is not installed, a message naming the service and what a consumer can act on. `Ready` continues to mean access and approval; `Provisioned` means delivery. Keeping them separate is what stops a transient apply failure from reading as a provider denial. The existing dependency and quota fan-outs have the opposite problem — they return raw errors and leave the entitlement reading `Ready=True` while nothing was provisioned — and should be brought to the same standard.

The ledger's record of kinds is load-bearing rather than informational: it is what teardown reads, because a withdrawn declaration says nothing at all about what it used to install. A declaration that stops resolving keeps its recorded kinds for the same reason.

Two failure modes must not be silent, because their silent forms have already been observed in the projection this generalizes. A kind the project's plane does not serve is reported, not treated as nothing to do. And the ledger records when provisioning was last evaluated, so a fan-out that has quietly stopped running is visible on the object rather than inferable from someone else's incident weeks later.

Mirroring the same summary onto the provider's `ServiceConsumer` record, so a provider can see which consumers are short of what was declared without asking them, is not built yet.

### Relationship to unconditional per-project seeding

Milo's project controller creates three per-project class objects at project creation — a gateway class, a DNS zone class, and a connector class — for every project, regardless of what it enables. They are created bare: no owner references, no labels, no teardown, no drift correction, and no failure reporting. Worse, they encode knowledge of specific services in the core project lifecycle, which is the coupling the service catalog exists to remove.

They should migrate, but not by weakening this mechanism. A project that has these objects today would lose them if it never enabled the corresponding service. The resolution is not to make provisioning unconditional; it is to make the *entitlement* unconditional for services every project is meant to have. "Seeded for every project" becomes "a service every project is entitled to by default" — a concept the catalog can express honestly, keeping one code path rather than two. Sequencing is a precondition, not a follow-up: default entitlements first, then adoption of the existing objects, then removal of the seeding.

## Production Readiness Review Questionnaire

Provisioning runs only for services whose `ServiceConfiguration` declares `spec.provisioning`; removing the section disables it for one service. Neither requires downtime, and re-enabling is idempotent because the write is a server-side apply of a fixed document.

The dominant operational risk is that the configuration is live and additions to a Published document are permitted, so a bad declaration reaches every consumer at once: it could install wrong objects or prune correct ones platform-wide, and workloads depending on a pruned class would fail to allocate. The signals that should inform a rollback are the count of entitlements in a non-provisioned state per service, the prune rate, and the age of the oldest entitlement not successfully evaluated. Consumers see the `Provisioned` condition and the per-resource ledger on their own entitlement; a `lastProvisioningEvaluation` timestamp makes a stalled fan-out visible. Metrics and events are not emitted yet, and cross-plane reconcile latency is not instrumented anywhere in this repository — it is the quantity that matters most here.

Cost is one configuration read per entitlement per resync, plus an apply per declared object, on top of a periodic resync across every engaged project cluster that already exists for location projection. Resource exhaustion is bounded by the per-configuration, per-declaration, and per-object caps, which are part of the security model rather than an operational concern. When the API server is unavailable, reconciles fail and retry and installed resources are unaffected.

## Implementation History

- 2026-08-17 — initial draft.
- 2026-08-17 — projection delivery, its two enforcement points, and the `Provisioned` ledger, with chainsaw suites proving delivery, gating, pruning, refusal with the webhook disabled, and project isolation against separately routed control planes.
- 2026-08-19 — the platform allowlist replaced by a provider-declared reference shape.
- 2026-08-19 — projection replaced by embedded objects. The provider writes the object it wants installed; the platform bounds the destination and the shape, not the content. The source-ownership bound goes with the source project.

## Drawbacks

One more bespoke controller solves the motivating case sooner, and the platform has three instances of the pattern so far — arguably not yet enough evidence to generalize. If the generic mechanism costs substantially more than a fourth hand-written reconciler, it is over-designed and should be cut back until it does not.

This is now, within a shape, a general write primitive: the provider decides the content and the platform decides only where it lands. The honest defence is that the projection's extra bound was narrow — it constrained where content came from, not what a consumer ended up holding — and cost a template language and a model of someone else's API to keep, both sources of error. The trade is defensible while the platform is the only provider and indefensible under third-party providers without a per-service write identity.

And the delivery path ships ahead of the authorization pairing it needs to be correct rather than merely functional. That is mitigated by reporting the gap per resource rather than leaving it implicit, but it is a real ordering cost.

## Alternatives

- **Projection from a provider-owned source project** — the previous design; see [Why an embedded object](#why-an-embedded-object).
- **An OCI or git bundle reference** — moves the authoritative content outside the API that governs it, adds a supply chain and a provenance problem, makes the consumer-visible failure "could not pull," and costs more to build than embedding for no additional bound.
- **A fourth bespoke controller for IP classes** — the honest benchmark, and the smallest change. It loses because it is the fourth instance of the same pattern, leaves the next case where this one started, and does nothing about the declaration gap the engagement enhancement identified as blocking access scoping.
- **Extending Milo's unconditional project seeding** — no approval gate, no teardown, no attribution, no drift correction, and it deepens core Milo's dependency on specific services. It also cannot express "compute needs this because it depends on networking."
- **Holding the entitlement out of Active until provisioning succeeds** — makes a broken project obviously broken, but conflates access with delivery and makes a transient apply failure indistinguishable from a provider denial.
- **Treating the operator ClusterRole as the ceiling** — needs no new machinery, but describes a control that is not in force against a `system:masters` identity. Rejected as unsound rather than insufficient.

## Open Questions

1. **Should the platform prevent name collisions between providers?** Two services can declare one kind under one name in one project, and the second to reconcile wins silently. A namespace-like prefix per service would prevent it and would take back the name the provider chose.
2. **Does a declaration change roll out to existing consumers immediately, or require acceptance?** Immediate matches today's behavior. Pinning or staged acceptance is safer and more machinery.
3. **Should the operator get a per-service write identity?** It would make audit attribution real and, more importantly, make target APIs' own authorization checks meaningful instead of vacuous. This is the largest gap in the security model, it is what would restore a bound on installing objects that name another provider's resources, and it is a Milo-side change.
4. **Should disabling a service be blocked while installed resources are in use**, or should the delete be issued and the owning API refuse to leave dangling state? This proposal recommends the latter.
5. **Should default-for-every-project entitlements exist?** The clean migration path for the existing unconditional seeding, and the catalog has no such concept today.
6. **First-party providers only, or third-party?** Under third-party providers the authorization gap stops being a caveat and becomes the thing to fix first, and per-service identity becomes a requirement rather than an improvement.
7. **Which Published configuration is active?** Provisioning and locations agree; quota does not. One rule should apply to all three.
8. **Who fixes cluster-scoped cleanup at project deletion?** A Milo bug that provisioning amplifies.

## References

- [Consumer project engagement](../consumer-project-engagement.md) — access scoping and cleanup that depend on this declaration
- [Locations as platform primitives](../locations-platform-primitive.md) — the hand-wired projection this generalizes
- [Service enablement](../service-enablement.md) — entitlements, approval, and dependencies
