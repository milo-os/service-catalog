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
  - [Why a reference and not a copy](#why-a-reference-and-not-a-copy)
  - [Why projection is the only delivery mode](#why-projection-is-the-only-delivery-mode)
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

A provider can now declare, in the `ServiceConfiguration` it already authors, what a project should be given when it enables the service. The platform installs those resources into every project holding an Active entitlement and removes them when the entitlement stops being Active. The provider declares once; every project that enables the service receives them, after any approval, with a consumer-visible statement of whether they arrived.

The declaration is deliberately narrow. A provider does not ship manifests. It names a project it owns, a kind, and a label selector over objects already in that project, and the platform projects *references* to them into entitled projects. That generalizes the mechanism the location-binding reconciler already runs for one hand-wired kind, rather than introducing a second, more powerful one beside it.

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
- Bound what a provider can install by a platform-owned decision rather than a provider-owned one, and do so without relying on RBAC that is not in force.

### Non-Goals

- Not a package manager. No dependency resolution among installed resources, no ordering graph, no rollback beyond convergence.
- Not a general-purpose renderer. There is no templating and no expression language.
- Not a way to change a project plane's API surface. What a project serves is resolved at the discovery layer.
- Not a way to grant permissions. Kinds that grant access or cause execution are refused outright; see [Security model](#security-model).
- Not consumer-configurable. A consumer supplies no values; enabling the service is the only input.

## Proposal

### What a provider declares

The networking team keeps its real ranges, and the classes describing them, in one project it owns, managed in version control. That does not change.

To make those classes reachable from consumer projects, the team adds a `spec.provisioning` block to the `ServiceConfiguration` it already publishes — the same document where it declares its metrics, its quota limits, and the location classes it runs on. Each entry names the source project, a kind and version, a label selector, and how its API spells a cross-project reference — for IPAM, `spec.source` keyed by `project` and `name`: *the classes in my project carrying this label should be usable from every project that enables me, and here is where a reference to one goes.* It names no consumer project, enumerates no classes, and writes no manifest.

Offering a new class later means adding a labelled object to the source project. Every already-entitled project picks it up, with no new configuration version, because the declaration is a selector rather than a list — the same reasoning `spec.locations.supportedClasses` already uses.

Compute declares networking as a dependency, as it already does. It inherits the classes without declaring anything, because a dependency entitlement is a real entitlement and provisions like any other.

The provider never learns how a project control plane is addressed, what credential writes into it, or how many consumers exist.

### What a consumer receives

A project admin enables networking — one `ServiceEntitlement`, or one `datumctl services enable`. If networking is `GatedByProvider`, nothing is installed while the request sits `PendingApproval`. Installation follows approval; it does not anticipate it.

Once the entitlement is Active, listing IP classes in the project returns the platform's classes. The project did not create them and cannot be missing them. They are visibly platform-owned: each is a reference naming the source project and the class within it, so the numbering, the ranges, and the lifetime stay where they belong and the project holds nothing.

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
2. **A new class reaches every existing project.** The networking team labels another class in its own project. Every already-entitled project sees it, with no consumer action and no republished configuration.
3. **Inheriting through a dependency.** A project enables compute. Compute depends on networking, a dependency entitlement is created, and once Active it provisions networking's resources into the same project. The consumer never explicitly enabled networking.
4. **A platform operator answers "why is this object here."** An object in a consumer project carries the service that installed it, the entitlement it was installed under, the declaration that produced it, and the source object it references — all answerable from the object alone.

### Notes/Constraints/Caveats

- **The active configuration is a live document.** `ServiceConfiguration` is mutable and unversioned. Published immutability is field-level, and *additions are always allowed*, so a provider can add a provisioning declaration to an already-Published configuration and have it reach every existing consumer. That is the status quo for quota, and it matters more once the declaration is more expressive. Within a declaration, the kind, the source project, and the reference shape are frozen once Published — changing any of them would silently re-point or rewrite everything already installed under that name — while the selector stays mutable, because adjusting which of a provider's own objects are offered is the intended way to reach existing consumers.
- **Configuration selection is inconsistent across fan-outs.** Provisioning takes the most recently created Published configuration, tie-breaking on name, matching the location reconciler; the quota fan-out takes the first Published one in list order. One rule should win.
- **Cross-plane events do not enqueue cleanly.** A provider's configuration edit converges in seconds, because the fan-out watches `ServiceConfiguration` on the root cluster. A newly labelled *source object* has no path to enqueue a consumer request and converges on a five-minute resync instead.
- **The operator engages every Ready project**, not only entitled ones. Narrowing that is the engagement enhancement's work.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| The mechanism becomes a general "write anything anywhere" primitive | What a projection can write is one reference — two platform-supplied strings at one path in `spec` — and only into projects that enabled the service, out of a project the service is published from. Enforced at admission and again in the controller, not left to RBAC, which is not an effective ceiling |
| Provisioning launders authorization past the target API's own checks | Not solved in this version; reported per resource on the entitlement rather than left implicit. See [The authorization gap](#the-authorization-gap) |
| A provider change silently reaches every consumer at once | Per-resource status on every entitlement; a rollout gate is an open question |
| A broad selector fans out multiplicatively | Caps on declarations per configuration and objects per declaration per project; exceeding one is refused and reported, never truncated |
| Teardown strands a project mid-way | Installed objects are owner-referenced to the cluster-scoped entitlement, so deleting it garbage-collects them; label-scoped pruning covers withdrawn declarations |
| Disabling a service removes resources still in use | The owning API, not service-catalog, refuses to leave dangling state — an open question |
| Cluster-scoped installed objects survive project deletion | Known gap in the project purger; called out rather than assumed away |

## Design Details

### Why a reference and not a copy

For each matching class in the source project, an entitled project receives an `IPClass` whose spec is a source reference: the source project and the class name, and nothing else. Three properties follow, and they are what "references, not copies" buys.

The consumer object carries no ranges and no numbering, so deleting or editing it cannot affect the platform's addressing. The reference is immutable in the IPAM API and chains of references are rejected, so a consumer cannot re-point it or build on top of it. And when the platform's class changes, nothing has to propagate — the reference already resolves through.

The installed object's name is derived from the canonical service name and the source object; the provider does not choose it. Two services therefore cannot contend for one object, and a service cannot shadow a name a consumer already relies on.

### Why projection is the only delivery mode

Every existing fan-out — billing, quota, locations — works by having the provider supply *values* into a schema the platform defined, never an object. The platform decides the target kind, the name, the ownership, and the fields. That is what makes the current trust boundary tractable, and a projection preserves it: the content is derived from objects that already exist in a plane where ordinary authorization applied when the provider created them.

Two alternatives were considered. **Inline manifests** in the configuration are the fastest and most expressive option, and they discard exactly that property — the set of installable kinds becomes whatever a provider writes, and there is no RBAC ceiling behind the API to catch the difference. They may return later in a bounded, literal form for resources that are not projections of anything. **An OCI or git bundle reference** moves the authoritative content outside the API that governs it, adds a supply chain and a provenance problem, makes the consumer-visible failure "could not pull," and grants strictly more trust than inline manifests while costing more to build.

### Security model

The services operator authenticates to Milo with a client certificate whose organization is `system:masters`, and reaches every project control plane by copying that config and rewriting the URL path. There is no per-project credential, no per-provider credential, and no impersonation. The operator's ClusterRole holds no grants for the kinds it installs, and could not usefully: a `system:masters` subject is not constrained by RBAC, and the kinds are named by providers at runtime. A per-kind grant would enforce nothing while implying a ceiling.

Two consequences are load-bearing. Anything a provider can express in a `ServiceConfiguration` is executed by an omnipotent identity in the consumer's control plane. And the bound therefore cannot be delegated to RBAC — a design that says "the ClusterRole is the ceiling" describes a control that is not in effect. Narrowing the operator's identity is worth doing on its own merits, but it is a Milo-side change and this design has to be correct without it.

**The shape of a projection.** The bound is what a projection can write, not a list of kinds it may write it to. The platform writes one object whose name it derives, whose labels and owner reference it sets, and whose `spec` is exactly one field holding two strings: the project the service is published from, and the name of an object the selector matched there. The provider says where that field is and what the two keys are called. It cannot supply a value, add a third key, write anything that is not a string, or reach outside `spec`.

That is enforced three times, and the repetition is not redundant. The `ServiceConfiguration` schema enforces it, so the API server refuses a malformed declaration with no webhook in the picture. Admission enforces it, so a provider gets a synchronous error rather than discovering the refusal in a consumer's status later. And the controller enforces it before every write, because a document admitted under an earlier schema stays in etcd.

**Which kinds are acceptable is the target API's decision.** A projection only works for a kind whose API supports a cross-project reference at all, and that API is the only thing that knows who may reference what. IPAM authorizes `IPClass.spec.source` with a `use` check at create, forbids a reference that states any policy of its own, and freezes it once created. A platform table restating that would be a second, worse copy of a rule it does not own — and the one that existed named the wrong API group for two months without anything catching it.

**No permission-granting kind can be constructed.** The earlier design carried a categorical deny list for RBAC objects, IAM policy bindings, service accounts, secrets, webhook configurations, and CRDs. Removing the table removes that stated bound, so the shape has to carry it instead. It does: granting access takes at least two independent pieces — a subject and a permission — and a projection writes one map of two strings at one path. `PolicyBinding` requires `roleRef`, `subjects` (a list), and `resourceSelector`; `iam` `Role` requires a bare top-level `launchStage`; RBAC bindings put `roleRef` and `subjects` outside `spec` entirely; a CRD requires `group`, `names`, `scope`, and `versions`. Each is refused by its own API for a missing required field. Secrets and service accounts are unreachable for a plainer reason: the group must be a dotted domain, so the core group cannot be named, and neither kind has a `spec` at all.

What remains is a residual, and it is worth stating plainly. A future API could define a kind whose entire spec is a single two-key reference that confers authority — an IAM binding shaped like `spec.subject{project,name}` would satisfy the shape. The bound then is that the reference names an object in the provider's own project, so what such a kind could grant is access to the provider's own resources, which is what offering a service means; and that the target API's create-time check is the thing deciding, evaluated against the operator. That last part is the authorization gap below, and it is why closing that gap matters more than a deny list did.

**Provider ownership of the source, and the pull direction, are the load-bearing bounds.** Three hold regardless of kind: a service may project only out of the project it is published from, only into projects that created an entitlement for it, and only once that entitlement is Active — after approval, for a `GatedByProvider` service. The first is the one the schema cannot express, because it is a fact about another object, so it lives in the webhook and in the controller and an e2e suite lands a violating document in etcd with the webhook disabled to prove the controller alone refuses it.

**Provider ownership of the source.** A projection may read only out of the producer project of the service the configuration describes, enforced at admission. Without it a provider could name any project as a source and have the platform read it, with an identity nothing would stop. For the same reason an empty selector matches nothing rather than everything: Kubernetes reads an empty selector as "match everything," and projecting a source project's entire contents by omission is never the intent.

**The trust arrow.** The consumer pulls; the provider does not push. A provider declares; nothing happens until a consumer creates an entitlement naming that service, and the fan-out runs only in the Active branch, after approval. A provider cannot enumerate or target projects. This property must survive every future extension — a proposal to let a provider select consumers, by label or organization or anything else, is a redesign of the trust model rather than a feature. Milo's quota system already has the weaker shape, where a grant-creation policy can target an arbitrary project chosen by a CEL expression; that is a precedent to avoid.

The one genuine push is that a provider adding a declaration affects consumers who enabled earlier, since additions to a Published configuration are always permitted. That is inherent to "the provider decides what its service needs," and it is why the bounds above concern *what* may be installed and not only *whether*.

### The authorization gap

This is the honest limit of the first version.

Where the target API authorizes a reference itself, it does so against whoever creates the object. IPAM does this well: creating a class that names another project's class triggers a subject access review for `use` on the source class, with the caller's scope rewritten to the source project, and it fails closed. But the caller here is the operator, whose identity passes that check trivially. The reference exists and works on the strength of the installer's authority rather than the consumer's — the project never held `use` on the platform's class. Revoking access later does not undo it, because the check runs at create, not at use.

Nothing in this version closes that. The gap is reported instead of hidden: each project's entitlement status carries `authorizationEstablished` per resource, with a consumer-facing explanation of what it means. It is false unconditionally, because nothing establishes a consumer grant for any kind — reporting it per kind would describe the target API's rigour rather than what the platform did.

Closing it requires the platform to establish a real grant — a platform-authored IAM binding, in a separate typed fan-out where the provider names only its own resources and the platform chooses the subject, the scope, and the verb — so the target API's own check would independently accept the write. That is separate work, and it is a prerequisite for the motivating case being correct rather than merely working.

### Lifecycle

| Event | Behavior |
| --- | --- |
| Provider publishes a changed declaration | The fan-out watches `ServiceConfiguration` on the root cluster; the desired set is re-derived, additions applied, removals pruned |
| Source object labelled or unlabelled in the source project | Converges on the next resync; no cross-plane event path exists |
| Consumer edits or deletes an installed object | Restored on the next reconcile — the object is a pointer at platform truth, and there is nothing in it a project is entitled to change |
| Entitlement leaves Active — rejected, revoked, deleting | Everything installed for it is torn down. Not-Active and deleting are treated identically |
| Entitlement deleted | Installed objects are owner-referenced to the cluster-scoped entitlement and garbage-collected |
| Declaration withdrawn | Pruned, scoped by service, entitlement, and declaration name, so one service's fan-out cannot remove another's objects. The kind comes from the entitlement's own ledger, which records what this project received, so teardown does not need the withdrawn declaration to still be there |
| Provisioning fails partway | Applied resources stay applied; the failure is retried and reported per resource |
| Target kind not served in the project plane | Reported as unprovisionable, not skipped |
| Project deleted | **Not reliably.** The project purger omits cluster-scoped resources, and both location bindings and IP classes are cluster-scoped. A pre-existing Milo gap that provisioning inherits |

Two of these are decisions rather than mechanisms. **Rollout** is immediate: a provider's edit reaches every consumer as soon as it converges, with no canary and no per-consumer pinning. That is the status quo for quota and locations, but the blast radius grows with the expressiveness of the declaration. **In-use resources at teardown** are not something service-catalog can reason about without learning per-service semantics; the division taken here is that service-catalog issues the delete and the owning API refuses to leave dangling state, surfacing as a stuck teardown with an honest message rather than silent corruption.

### Failure legibility

A project that did not get its resources says so, in its own control plane, in terms of the service that owed them.

`ServiceEntitlement` gains a `Provisioned` condition alongside `Ready`, and a per-resource ledger in status: for each declaration, the kind, how many objects it resolved to, its state — installed, failed and retrying, or unprovisionable — and, when it is not installed, a message naming the service and what a consumer can act on. `Ready` continues to mean access and approval; `Provisioned` means delivery. Keeping them separate is what stops a transient apply failure from reading as a provider denial. The existing dependency and quota fan-outs have the opposite problem — they return raw errors and leave the entitlement reading `Ready=True` while nothing was provisioned — and should be brought to the same standard.

Two failure modes must not be silent, because their silent forms have already been observed in the projection this generalizes. A kind the project's plane does not serve is reported, not treated as nothing to do. And the ledger records when provisioning was last evaluated, so a fan-out that has quietly stopped running is visible on the object rather than inferable from someone else's incident weeks later.

Mirroring the same summary onto the provider's `ServiceConsumer` record, so a provider can see which consumers are short of what was declared without asking them, is not built yet.

### Relationship to unconditional per-project seeding

Milo's project controller creates three per-project class objects at project creation — a gateway class, a DNS zone class, and a connector class — for every project, regardless of what it enables. They are created bare: no owner references, no labels, no teardown, no drift correction, and no failure reporting. Worse, they encode knowledge of specific services in the core project lifecycle, which is the coupling the service catalog exists to remove.

They should migrate, but not by weakening this mechanism. A project that has these objects today would lose them if it never enabled the corresponding service. The resolution is not to make provisioning unconditional; it is to make the *entitlement* unconditional for services every project is meant to have. "Seeded for every project" becomes "a service every project is entitled to by default" — a concept the catalog can express honestly, keeping one code path rather than two. Sequencing is a precondition, not a follow-up: default entitlements first, then adoption of the existing objects, then removal of the seeding.

## Production Readiness Review Questionnaire

Provisioning runs only for services whose `ServiceConfiguration` declares `spec.provisioning`; removing the section disables it for one service. Neither requires downtime, and re-enabling is idempotent because the content is derived rather than authored.

The dominant operational risk is that the configuration is live and additions to a Published document are permitted, so a bad declaration reaches every consumer at once: it could install wrong references or prune correct ones platform-wide, and workloads depending on a pruned class would fail to allocate. The signals that should inform a rollback are the count of entitlements in a non-provisioned state per service, the prune rate, and the age of the oldest entitlement not successfully evaluated. Consumers see the `Provisioned` condition and the per-resource ledger on their own entitlement; a `lastProvisioningEvaluation` timestamp makes a stalled fan-out visible. Metrics and events are not emitted yet, and cross-plane reconcile latency is not instrumented anywhere in this repository — it is the quantity that matters most here.

Cost is one configuration read and one source list per entitlement per resync, plus an apply per declared resource, on top of a periodic resync across every engaged project cluster that already exists for location projection. Resource exhaustion is bounded by the per-configuration and per-declaration caps, which are part of the security model rather than an operational concern: without them a broad selector over a large source project fans out multiplicatively across every entitled project. When the API server is unavailable, reconciles fail and retry and installed resources are unaffected.

## Implementation History

- 2026-08-17 — initial draft.
- 2026-08-17 — projection delivery, its two enforcement points, and the `Provisioned` ledger, with chainsaw suites proving delivery, gating, pruning, refusal with the webhook disabled, and project isolation against separately routed control planes.
- 2026-08-19 — the platform allowlist replaced by a provider-declared reference shape. The provider names the kind, the version, and where a reference goes; the target API decides whether it will accept one. The IPAM entry moves to the API it was always meant to name, `ipam.miloapis.com/v1alpha1`.

## Drawbacks

One more bespoke controller solves the motivating case sooner, and the platform has three instances of the pattern so far — arguably not yet enough evidence to generalize. If the generic mechanism costs substantially more than a fourth hand-written reconciler, it is over-designed and should be cut back until it does not.

This also creates a capability whose safe form is narrow and whose unsafe form is a small edit away, on a foundation with no enforcement behind the API. Every future request — "can I just install one manifest," "can I template one more field," "can I target consumers by label" — moves it toward the unsafe form, and each will be individually reasonable. A mechanism that requires ongoing discipline to stay safe is worse than one that is safe structurally, and this one requires discipline until the write identity is narrowed.

And the delivery path ships ahead of the authorization pairing it needs to be correct rather than merely functional. That is mitigated by reporting the gap per resource rather than leaving it implicit, but it is a real ordering cost.

## Alternatives

- **Inline free-form manifests as the primary mode** and **an OCI or git bundle reference** — see [Why projection is the only delivery mode](#why-projection-is-the-only-delivery-mode).
- **A fourth bespoke controller for IP classes** — the honest benchmark, and the smallest change. It loses because it is the fourth instance of the same pattern, leaves the next case where this one started, and does nothing about the declaration gap the engagement enhancement identified as blocking access scoping.
- **Extending Milo's unconditional project seeding** — no approval gate, no teardown, no attribution, no drift correction, and it deepens core Milo's dependency on specific services. It also cannot express "compute needs this because it depends on networking."
- **Holding the entitlement out of Active until provisioning succeeds** — makes a broken project obviously broken, but conflates access with delivery and makes a transient apply failure indistinguishable from a provider denial.
- **Treating the operator ClusterRole as the ceiling** — needs no new machinery, but describes a control that is not in force against a `system:masters` identity. Rejected as unsound rather than insufficient.

## Open Questions

1. **Should a projection be able to state anything beyond a reference?** Deliberately not, today: two platform-supplied strings at one path. Any widening — a literal value, a second reference, a copied field — is what turns this into a templating engine and puts the burden of bounding it back on the platform.
2. **Does a declaration change roll out to existing consumers immediately, or require acceptance?** Immediate matches today's behavior. Pinning or staged acceptance is safer and more machinery.
3. **Should the operator get a per-service write identity?** It would make audit attribution real and, more importantly, make target APIs' own authorization checks meaningful instead of vacuous. This is the largest gap in the security model, and it is a Milo-side change.
4. **Should disabling a service be blocked while installed resources are in use**, or should the delete be issued and the owning API refuse to leave dangling state? This proposal recommends the latter.
5. **Should default-for-every-project entitlements exist?** The clean migration path for the existing unconditional seeding, and the catalog has no such concept today.
6. **First-party providers only, or third-party?** Under third-party providers the authorization gap stops being a caveat and becomes the thing to fix first, and per-service identity becomes a requirement rather than an improvement.
7. **Which Published configuration is active?** Provisioning and locations agree; quota does not. One rule should apply to all three.
8. **Who fixes cluster-scoped cleanup at project deletion?** A Milo bug that provisioning amplifies.

## References

- [Consumer project engagement](../consumer-project-engagement.md) — access scoping and cleanup that depend on this declaration
- [Locations as platform primitives](../locations-platform-primitive.md) — the hand-wired projection this generalizes
- [Service enablement](../service-enablement.md) — entitlements, approval, and dependencies
