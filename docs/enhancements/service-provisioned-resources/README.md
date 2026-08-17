---
status: provisional
stage: alpha
latest-milestone: "v0.x"
---

# Service-Provisioned Resources

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [What a provider experiences](#what-a-provider-experiences)
  - [What a consumer experiences](#what-a-consumer-experiences)
  - [User Stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [What already exists](#what-already-exists)
  - [Declaration: how a provider expresses what to install](#declaration-how-a-provider-expresses-what-to-install)
  - [The worked example](#the-worked-example)
  - [Templating and per-project values](#templating-and-per-project-values)
  - [Lifecycle](#lifecycle)
  - [Ownership and drift](#ownership-and-drift)
  - [Security model](#security-model)
  - [Failure legibility](#failure-legibility)
  - [Relationship to unconditional per-project seeding](#relationship-to-unconditional-per-project-seeding)
- [Prior art](#prior-art)
- [Recommendation](#recommendation)
  - [What to build first](#what-to-build-first)
  - [Open questions needing a human decision](#open-questions-needing-a-human-decision)
  - [What could not be determined](#what-could-not-be-determined)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)

## Summary

A service is rarely usable the moment it is switched on. Networking is the clearest case: a project that enables networking cannot allocate an address until an address class exists in its own control plane, and nothing creates one. Every consumer of the API hand-writes fixtures to test against, and no real project has any. The service that owns the concept has no way to say "a project using me needs this."

This proposes that a provider be able to declare, in the `ServiceConfiguration` it already authors, a set of resources the platform installs into a consumer project when that project's entitlement becomes Active, and removes when it stops being Active. The provider declares once; every project that enables the service receives them, after approval, with a consumer-visible statement of whether they arrived.

The recommended shape is deliberately narrow. A provider does not ship manifests. A provider names, by label selector, objects it already owns in a project it owns, and the platform projects *references* to them into entitled projects. That is not a new idea here — it is what the location-binding reconciler already does for locations, hand-written for one kind, and it is the shape the IPAM API already models natively through a class that names a class in another project. This proposal generalizes one existing mechanism rather than introducing a second, more powerful one beside it.

It also argues that the security work is the substance of this enhancement rather than a section of it. The services operator today authenticates to Milo with a certificate carrying the `system:masters` organization and reaches every project control plane by URL path rewrite. There is no per-provider credential, no per-project credential, and no effective RBAC ceiling. Any provisioning capability built on that foundation is only as bounded as the controller code chooses to be, so the bounds have to be designed in rather than assumed.

## Motivation

Three problems converge here.

**The concrete one.** IP classes do not exist in real projects. The platform owns the ranges; a project needs to name a class to allocate from one. There is no mechanism that puts anything in a project because of a service, so classes are only ever seen in test fixtures — where tenancy is simulated with impersonation kubeconfigs. Compute inherits the same gap through its dependency on networking.

**The structural one.** `ServiceEntitlement` reconciliation has exactly two fanouts — dependency enrollment and quota grants — plus a separate reconciler that projects locations. There is no extension point: no plugin, no hook, no generic path. Each new "when a project enables this service, it also needs X" has meant a new hand-written controller with its own naming, labelling, pruning, and mostly absent status reporting. There is no reason to expect the list of Xs to stop growing, or for each one to re-derive the same answer to "what happens when the entitlement is deleted."

**The trust one.** This is a capability for one party to cause writes into another party's control plane. That is already true of quota grants and location bindings, but they are bounded tightly enough that the trust is easy to miss. Generalizing the capability without generalizing the bounds is the real risk in this design, and most of the detail below is about that.

There is also a decision already on record pointing here. The consumer-project-engagement enhancement states the intended end state directly: the catalog should be the authoritative declaration of the resource types a service manages in a consumer project, so that discovery, approval-time governance, access scoping, and cleanup all derive from one published fact; and a service's access should be scoped so it may act only in projects where it has an active consumer, and only on the types it declared. Its `ManagedResources` operator config is explicitly labelled an interim stand-in for that catalog declaration. This enhancement is the catalog half of that plan.

### Goals

- Let a provider declare, in the artifact it already authors, what a consumer project needs in order to use its service.
- Deliver those resources only to projects that enabled the service, and only after any provider approval.
- Remove them when the service stops being enabled, without every provider writing its own teardown.
- Tell a consumer, in terms of the service that owed them, when resources did not arrive.
- Make the declaration the authoritative statement of the resource types a service manages in a consumer project, closing the gap the engagement enhancement flags.
- Bound what a provider can install by a platform-owned decision rather than a provider-owned one, and do so without relying on RBAC that is not currently in force.
- Replace the bespoke location projection with this mechanism, as proof it is general enough.

### Non-Goals

- Not a package manager. No dependency resolution among installed resources, no ordering graph, no rollback beyond convergence.
- Not a general-purpose renderer. Templating is deliberately closed and there is no plan to open it.
- Not a way to install CRDs or otherwise change a project plane's API surface. What a project serves is a control-plane concern resolved at the discovery layer.
- Not a way to grant permissions. Installing RBAC or IAM bindings through this mechanism is out of scope; [Security model](#security-model) explains why and what should happen instead.
- Not consumer-configurable. A consumer supplies no values; enabling the service is the only input.
- Not a fix for cross-plane engagement scoping. That is the engagement enhancement's job; this one supplies the declaration it needs.

## Proposal

### What a provider experiences

The networking team keeps its real ranges, and the classes describing them, in one platform project, managed in version control. That does not change.

To make those classes reachable from consumer projects, the team adds a block to the `ServiceConfiguration` it already publishes — the same document where it declares its metrics, its quota limits, and the location classes it runs on. The block says, in effect: *the classes in my platform project carrying this label should be usable from every project that enables me.* It names a source project it owns, a kind, and a selector. It does not name any consumer project, does not enumerate the classes, and does not write a manifest.

Adding a class later means adding a labelled object to the platform project. Every already-entitled project picks it up, with no new configuration version, because the declaration is a selector rather than a list. That is the same reasoning `spec.locations.supportedClasses` already uses — classes rather than names, so new PoPs reach entitled projects without republishing.

Compute declares networking as a dependency, as it already does. It inherits the classes without declaring anything, because a dependency entitlement is a real entitlement and provisions like any other.

The provider never learns how a project control plane is addressed, what credential writes into it, how many consumers exist, or which are new. It sees, on each `ServiceConsumer` record in its own project, whether that consumer received what was declared.

### What a consumer experiences

A project admin enables networking — one `ServiceEntitlement`, or one `datumctl services enable`. If networking is `GatedByProvider`, nothing is installed while the request sits `PendingApproval`. Installation follows approval; it does not anticipate it. This is already how dependencies and quota grants behave.

Once the entitlement is Active, listing IP classes in the project returns the platform's classes. The project did not create them and cannot be missing them. They are visibly platform-owned: each is a reference naming the platform project and the class within it, so the numbering, the ranges, and the lifetime stay where they belong and the project holds nothing.

If something did not arrive, the entitlement says so, in the project's own control plane, naming the service that owed it:

```
Provisioned  False  PartiallyProvisioned
  2 of 3 resources installed by networking.datumapis.com.
  IPClass "public-unicast" was not installed: the source class is not shared.
```

The consumer does not discover the problem later as an unrelated component reporting a symptom. That is not hypothetical — when location projection stopped in staging, the visible signal was compute reporting that no locations were registered with the system: a downstream component describing a failure that belonged to a different service's provisioning path, weeks after the fact. Producing an honest, local, service-attributed signal is a first-class goal here.

Disabling the service removes what was installed.

### User Stories

#### Story 1 — classes reach a real project

A project enables networking and can immediately allocate an address from a platform-managed class, without anyone hand-creating a class in that project and without the project being able to invent its own numbering.

#### Story 2 — a new class reaches every existing project

The networking team adds a class to the platform project. Every already-entitled project sees it within a bounded interval, with no consumer action and no republished configuration.

#### Story 3 — a provider sees who did not get what they were owed

A provider reads its `ServiceConsumer` records and sees which consumers are fully provisioned and which are not, with reasons, before those consumers file a ticket.

#### Story 4 — inheriting through a dependency

A project enables compute. Compute depends on networking, a dependency entitlement is created, and once Active it provisions networking's resources into the same project. The consumer never explicitly enabled networking.

#### Story 5 — a platform operator answers "why is this object here"

An operator looking at an object in a consumer project can tell, from the object alone, which service installed it, under which entitlement, and from which configuration.

### Notes/Constraints/Caveats

- **The active configuration is a live document.** `ServiceConfiguration` is mutable and unversioned; `spec.version` is decorative and has no semantic meaning. Published immutability is field-level only — entries cannot be removed or have key fields changed, but *additions are always allowed*. A provider can therefore add a provisioning declaration to an already-Published configuration and have it reach every existing consumer on the next reconcile. That is the status quo for quota, and it matters more once the declaration is more expressive.
- **Configuration selection is currently inconsistent.** The location reconciler picks the most recently created Published configuration, tie-breaking on name; the quota fanout picks the first Published one in list order. A new fanout has to choose, and the two existing ones should be reconciled to that choice.
- **Cross-plane events do not enqueue cleanly.** The location reconciler falls back to a five-minute resync precisely because root-cluster changes cannot enqueue project-scoped requests. Any provisioning mechanism inherits a convergence latency rather than an event-driven one.
- **Projected objects carry references, not data.** A consumer project holding a reference to a platform class does not hold the range, cannot renumber it, and needs no reconciliation when the range changes.
- **The operator engages every Ready project**, not only entitled ones. Narrowing that is the engagement enhancement's work, not this one's, but it is part of why the identity question below is urgent.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| The mechanism becomes a general "write anything anywhere" primitive | Platform-owned allowlist of installable kinds, enforced at admission and again in the controller — not left to RBAC, which is not currently an effective ceiling |
| Provisioning launders authorization past the target API's own checks | Where the target API performs its own permission check, the platform must establish a real grant rather than rely on writing as an omnipotent identity |
| A provider change silently reaches every consumer at once | Per-resource status on every entitlement and consumer record; a rollout decision is an open question |
| Templating grows into a renderer | Closed substitution set, no expression language; new needs are met with new typed fields |
| Teardown strands a project mid-way | Entitlement-owned objects in the same plane are garbage-collected; everything else is label-pruned, following the two existing precedents |
| Disabling a service removes resources still in use | The owning API, not service-catalog, refuses to leave dangling state — an open question |
| Cluster-scoped installed objects survive project deletion | Known gap in the project purger; called out rather than assumed away |

## Design Details

### What already exists

Being precise about the existing machinery matters, because the pattern is unusually consistent and any proposal either extends it or must justify breaking it.

**`ServiceConfiguration` is the single provider-authored document**, cluster-scoped and Platform-context, one per `Service` by convention. Three separate fanouts read it:

- **Billing.** The provider declares metrics and monitored resource types; the platform produces billing objects on the root plane. Admission forces every declared name to carry the provider's own canonical service-name prefix.
- **Quota.** The provider declares limits — a metric, a consumer kind, a default and a maximum. On entitlement activation the platform writes one `ResourceGrant` per limit into the consumer project, in a fixed platform namespace, with a name derived by hashing the service name, the project, and the limit name. The write is a server-side apply with a fixed field manager and force-ownership; there is no owner reference, so lifecycle is label-scoped pruning.
- **Locations.** The provider declares which location classes it supports. A per-project reconciler projects platform `Location` objects into entitled projects as consumer-facing `LocationBinding` objects, behind three gates, gated additionally on the entitlement being Active, owner-referenced to the cluster-scoped entitlement so deletion garbage-collects them, labelled for pruning, and written with a distinct field manager.

The shape shared by all three is the important part:

> **The provider supplies values in a schema the platform defined. The platform decides the target kind, the namespace, the name, the ownership, and the fields.**

Not one of them lets a provider supply an object. That is not an accident of implementation — it is what makes the current trust boundary tractable.

There is also an explicit statement of the extension pattern. The plan that produced today's `ServiceConfiguration` scoped it to what billing consumes and said that future consumers — quota, entitlements, and others — would each get their own top-level section on `ServiceConfiguration` and their own fanout reconciler. Quota subsequently arrived exactly that way. This proposal follows the same route.

**How a project plane is reached.** The services operator runs two managers: a plain manager against the Milo root, and a multicluster manager backed by the Milo provider. That provider watches `Project` objects and, for each Ready project, copies the operator's own rest config and rewrites the URL path to the project's control-plane subpath, then engages the resulting cluster under the project's name. On the server side a filter strips that subpath, records the project, and injects IAM parent extras into the authenticated user before authorization runs; storage is isolated by a per-project etcd key prefix.

**What credential writes.** There is no per-project or per-provider credential anywhere in this path. The project client is the operator's own config with a different URL. That config authenticates with a client certificate whose organization is `system:masters` — cluster-admin-equivalent on Milo and, by path rewrite, in every project control plane. The operator's ClusterRole enumerates the kinds it writes, but given the `system:masters` identity that enumeration is documentation rather than an enforced ceiling. This is the single most important fact for the design, and it is developed in [Security model](#security-model).

**The IPAM side of the motivating case.** The relevant kind is `IPClass` in the IPAM group: cluster-scoped, with tenancy expressed by the storage key prefix derived from the caller's IAM parent extras rather than by namespaces. It already supports exactly the shape the motivating case wants — a class whose spec names a class in another project by project and name, with every other field required to be empty. The reference is immutable, chains of references are rejected, and pools may not name a reference class. Creation of a cross-project reference is authorized by a subject access review for the verb `use` on the source class, issued with the caller's parent scope rewritten to the source project so the question asked is "may you use *that* project's class" rather than one the caller trivially answers yes to. The checker fails closed when absent. A "platform project" holding real ranges is an operational convention in fixtures and e2e suites, not a typed concept — and the tenancy code explicitly says it must not become a special case.

This is a considerable gift to the design: **the motivating case needs no new API kind.** The object to install in a consumer project is an `IPClass` whose spec is a reference to the platform project's class. It is also a warning, developed below: that `use` check is evaluated against whoever creates the object, and the platform's identity passes it trivially.

### Declaration: how a provider expresses what to install

Three mechanisms are worth taking seriously.

#### Option A — inline manifests in the ServiceConfiguration

The provider embeds the objects to install, verbatim, in the configuration.

*For.* It is immediately obvious what will be created. The declaration is atomic with the rest of the configuration: one document, one review, one publish. There is no second supply chain, no registry to reach, no credential to hold. It covers every candidate use case — default policies, starter resources, a default gateway class — without further design, and it is by far the fastest thing to build.

*Against.* It discards the property that makes the existing fanouts safe. The set of installable kinds becomes whatever a provider writes; naming and namespacing become provider-chosen, so two providers can collide and a provider can shadow something a consumer relies on. `ServiceConfiguration` becomes a manifest store, with the object-size and diff-legibility problems that implies, and the field-level Published-immutability rules — designed for declarations — fit payloads badly, while the rule that additions are always allowed becomes considerably more consequential. It also creates immediate pressure for templating, because a manifest that cannot reference the project it lands in is not very useful, which is how a data field becomes a renderer.

Crucially, under the current identity model there is nothing behind the API to catch a mistake. Inline manifests are not rejected outright below — they are rejected *as the primary mode*, and admitted later in a bounded, allowlisted form for resources that are not projections of anything.

#### Option B — a reference to an OCI bundle or git source

The provider publishes a bundle; the configuration points at it by tag or digest.

*For.* It matches how platform component manifests already ship, so the tooling and mental model exist. It scales to large sets, versions independently of the catalog, and keeps `ServiceConfiguration` small.

*Against.* It moves the authoritative content outside the API that governs it. A configuration can read Published while the content behind a tag has changed — and since the catalog's phase model is already weak on versioning, this compounds an existing problem rather than working around it. The platform acquires registry credentials, network egress, and a provenance problem it does not have. Failure legibility gets materially worse: the consumer-visible failure becomes "could not pull," far from anything either party can see. And relative to Option A it grants *more* trust — an opaque payload rather than a reviewable one — while costing more to build.

Digest pinning fixes the mutability objection and none of the others. This is the weakest option for a first version and the one most likely to be right eventually, if provisioned content ever becomes large. It is not large now.

#### Option C — typed projection of platform-owned objects (recommended)

The provider names a source project it owns, a kind, and a label selector. The platform installs, in every entitled project, an object of an allowlisted kind that *references* each matching source object.

*For.* It is bounded by construction. The content is not authored in the configuration; it is derived from real objects that already exist in a plane where ordinary authorization applied when the provider created them. The target kind is chosen by the platform from an allowlist, not by the provider. The selector gives the provider the dynamism they need — add a labelled object, every entitled project sees it — without a republish, which is the argument the location configuration already makes for class selectors over names. It is a straight generalization of a mechanism already running for locations, so naming, labelling, pruning, ownership, and gating semantics are known rather than invented. It matches the settled shape for the motivating case exactly: the consumer receives references, the platform owns the numbering, the project owns nothing. And for the motivating case the target kind and its reference semantics already exist in the IPAM API, including immutability of the reference and rejection of chains.

*Against.* It only covers resources that *are* projections of something. A default policy, a starter resource, or a per-project default with no platform-side original cannot be expressed. Where the target API has no native cross-project reference — as `Location` does not, which is why `LocationBinding` exists — it requires a paired consumer-facing kind, which is real API work owned by the service rather than by service-catalog. And it puts the provider's source project on the trust path: the platform must be sure the provider owns the project it projects out of.

#### Recommendation on declaration

**Option C as the primary and only initial mode, with Option A admitted later in a bounded form under the same allowlist, for resources that are not projections of anything.** Option B is rejected for now and revisited only if provisioned content outgrows an API object.

Concretely: one new `spec.provisioning` section on `ServiceConfiguration` — following the established "new top-level section plus sibling fanout" pattern — containing a bounded list of resource declarations. Each declares a source that is either a projection (a selector over a provider-owned source project, plus the allowlisted consumer-facing kind to install) or, in a later phase, a literal object of an allowlisted kind. Both go through one delivery path, one allowlist, one ownership model, and one status ledger; the only difference is where the content comes from.

Two points carry the security argument and should be stated as invariants rather than details:

- The consumer-facing kind is chosen from a platform-owned allowlist that also declares, per kind, which fields may be populated and from where. It is never chosen freely by the provider.
- The source project must be verifiably owned by the provider — naturally, the service's own producer project, or a project provably held by the same producer — enforced at admission, where the existing validating webhooks already do cross-reference integrity work of this kind.

### The worked example

Networking's configuration gains a provisioning declaration naming its platform project, the IPAM class kind, and a label selector over the classes it intends to offer. For each matching class in that project, every entitled project receives an `IPClass` whose spec is a source reference to it: the same name, the platform project, and nothing else.

Three properties of that outcome are worth naming, because they are what "references, not copies" buys:

- The consumer object carries no ranges and no numbering. Deleting or editing it cannot affect the platform's addressing.
- The reference is immutable in the IPAM API, and chains are rejected, so a consumer cannot re-point it or build on top of it.
- When the platform's class changes, nothing needs to propagate. The reference already resolves through.

Compute's entitlement produces a dependency entitlement for networking, which provisions the same set into the same project. Nothing in compute's configuration mentions classes.

The one thing this example does *not* get for free is authorization, and that is developed below.

### Templating and per-project values

Installed resources need the project's own identity and references to platform-owned objects elsewhere. That is a real requirement, and it is also exactly where a bounded mechanism becomes an unbounded one. Crossplane is the cautionary tale: patches-and-transforms began as a small closed substitution set and grew, case by defensible case, until composition functions running arbitrary code became necessary.

The line proposed here is **substitution over a closed, platform-defined set of values, with no expression language.**

Available values:

- the consumer project's name and UID
- the entitlement's name and UID
- the service's canonical name
- the source project's name and the source object's name, in projection mode
- in projection mode only, a set of source-object field paths that the **platform** declares per target kind — not the provider

No conditionals, no loops, no functions, no arbitrary lookups, and no consumer-supplied values, because the consumer supplies nothing.

The last item deserves emphasis, because it is where the location projection already sits. That reconciler copies the source `Location`'s topology map verbatim onto the binding, and the copy is load-bearing: downstream consumers read those keys, and an empty topology silently breaks location-scoped deploys. So verbatim field copying is necessary in the general case. But *which* fields may be copied must be a property of the target kind, declared by the platform alongside the allowlist, not a path the provider writes into its configuration. Otherwise "copy this field" becomes a read primitive over provider-controlled objects with platform-controlled destinations. The motivating case needs none of this — a source reference is two strings — which is another reason to start there.

The test for whether the line is holding: **a reviewer reading a `ServiceConfiguration` should be able to enumerate the complete set of objects that will exist in a consumer project, and their contents, without running anything.** When a new requirement cannot be met inside that constraint, the answer is a new typed field — the way `spec.locations` was a new typed field — not a new template capability. Note that this constraint is doing more work than usual here, because it is currently the *only* review gate: there is no per-provider RBAC behind it to catch what review misses.

### Lifecycle

| Event | Behavior | Converges on its own? |
| --- | --- | --- |
| Provider publishes a changed declaration | The active configuration is re-read each reconcile; the desired set is re-derived, additions applied, removals pruned | Yes, but with no staging — see below |
| Consumer edits an installed object | Platform-managed fields restored on the next resync (`Managed` mode) | Yes |
| Consumer deletes an installed object | Recreated on the next resync (`Managed`) or left absent (`Adopted`) | Yes |
| Service disabled / entitlement deleted | Objects in the consumer plane owner-referenced to the cluster-scoped entitlement are garbage-collected, as location bindings already are; anything not owner-referenceable is label-pruned in the finalizer, as quota grants already are | Yes |
| Entitlement leaves Active — rejected, revoked | Same teardown. The location reconciler already treats not-Active and deleting identically, which is the correct precedent | Yes |
| Source object deleted or unlabelled in the platform project | Pruned from every consumer, as a removed availability record already prunes a binding | Yes |
| Provisioning fails partway | Applied resources stay applied; the failing one retries with backoff; the entitlement reports partial provisioning per resource | Yes, if transient |
| Project deleted | **Not reliably.** See below | No |
| Target kind not served in the project plane | Must be reported, not skipped | No — needs a decision |

Five of these need a decision rather than a mechanism.

**Rollout of a declaration change.** The active configuration is a live, mutable document, and additions to a Published one are always permitted. A provider's edit therefore reaches every consumer within one resync, with no canary, no staging, and no per-consumer pinning. That is the status quo for quota and locations, so this proposal does not make it worse — but it makes it matter more, because the blast radius grows with the expressiveness of the declaration. The options are to accept it, to pin each entitlement to the configuration state it was provisioned against and require an explicit bump, or to adopt an approval step for changes affecting existing consumers. Open question.

**Project deletion does not clean up cluster-scoped objects.** The project purger discovers and deletes namespaced resources and namespaces, and explicitly omits cluster-scoped resources. It runs in a state machine alongside a best-effort delete of the project control plane that is issued *first*, with no ordering guarantee between them. Both the location bindings that exist today and the IPAM classes the motivating case would install are cluster-scoped, so neither is individually deleted; they die only when the underlying plane or keyspace goes away. Whether that is acceptable depends on the plane model in use, and in the shared-apiserver model no code was found that reclaims a deleted project's key prefix at all. This is a pre-existing gap that provisioning inherits and amplifies — it should be named in the design and fixed in Milo, not worked around here.

**Kind not served.** The location reconciler tolerates the target kind being absent and treats it as nothing to do. For a generic mechanism that is the wrong default: "this project's plane does not serve the kind the service says you need" is precisely what the consumer must be told. Report it as an explicit unprovisionable reason.

**In-use resources at teardown.** Deleting a class out from under allocations that reference it is a data-integrity problem service-catalog cannot reason about without learning per-service semantics. The right division is that service-catalog issues the delete and the owning API refuses to leave dangling state, through its own validation or finalizers, which surfaces as a stuck teardown with an honest message rather than silent corruption. Whether disablement should instead be blocked up front is an open question. Note also a pre-existing teardown bug in this area: entitlement finalization deletes the shared `ServiceConsumer` unconditionally rather than reference-counting the last backing entitlement, which provisioning inherits and which is already tracked.

**Delivery versus access.** If provisioning fails, the entitlement should not be reported as not-Active. `Ready` — the consumer has access, and any approval was granted — and `Provisioned` — the resources arrived — are different facts and should be different conditions. Conflating them would make a transient apply failure look like a denial, which is exactly the misattribution this design exists to eliminate. This also matters because the existing fanouts have the opposite problem: a failed dependency or quota grant returns a raw error and leaves the entitlement reading `Ready=True`, so the failure is invisible on the object. Provisioning should not repeat that, and the existing fanouts should adopt the same treatment.

### Ownership and drift

Two defensible positions and one hybrid.

**Platform-owned and reconciled.** The platform restores its fields on every resync. A consumer who edits an installed object sees the edit revert within the resync interval, with no explanation unless one is provided. That is acceptable only if the object is visibly not theirs — which is exactly true of a projected reference. An `IPClass` that is a source reference is a pointer at platform truth; there is nothing in it a project is entitled to change, and letting a project edit it would be letting a project re-point at someone else's ranges. The IPAM API already agrees: the reference is immutable there. For projections this is the only correct answer.

**Handed over and left alone.** The platform creates the object if absent and never touches it again. Right for starter and sample resources, where the point is that the consumer owns them. The costs are real: a provider's later improvement never reaches existing projects, and the platform cannot distinguish "the consumer customized this" from "the consumer never touched it," so it cannot make a sensible upgrade decision later.

**Field-level co-ownership.** Server-side apply with a per-service field manager gives the platform ownership of the fields it declares and leaves the rest to the consumer. This is the most correct and the most subtle, and it is already how quota grants are written. It fits objects with a platform-owned core and a consumer-owned periphery.

**Proposal:** a per-resource ownership mode with two values — `Managed` (server-side apply, platform-declared fields authoritative, removed when the entitlement stops being Active) and `Adopted` (created if absent, never updated, never removed). Default `Managed`. Projection mode is always `Managed` and the option does not apply. The vocabulary is borrowed deliberately from the Kubernetes addon manager's reconcile-versus-ensure-exists modes, which is the same distinction with a decade of operational validation.

Whichever mode, an installed object should say what installed it. The existing fanouts label by managed-by and by entitlement; extending that to the service's canonical name and the installing configuration makes "why is this here and who do I ask" answerable from the object alone. The engagement enhancement independently requires the service-name label for scoped teardown, and specifically warns against pruning by the shared managed-by label — the two designs should use one labelling scheme, not two.

### Security model

This is a capability for one party to cause writes into another party's control plane. It deserves the most scrutiny in this document, and the conclusion is that the security work is the enhancement, not a caveat on it.

#### Start from what is actually in force

The services operator authenticates to Milo with a client certificate whose organization is `system:masters`. It reaches every project control plane by copying that same config and rewriting the URL path. There is no per-project credential, no per-provider credential, no impersonation, and no token exchange. The operator's ClusterRole enumerates the kinds it writes, but a `system:masters` subject is not constrained by RBAC, so that enumeration documents intent rather than enforcing a ceiling. The multicluster provider engages every Ready project, not only entitled ones.

Two consequences follow, and both are load-bearing.

First, **anything a provider can express in a `ServiceConfiguration` is executed by an omnipotent identity in the consumer's control plane.** The bound on what a provider can cause is, today, exactly the set of things the controller code chooses to write. That is a code invariant, not a security control, and it will not survive a mechanism whose entire purpose is to make what gets written configurable.

Second, **an allowlist cannot be delegated to RBAC.** A design that says "the ClusterRole is the ceiling" would be describing a control that is not in effect. The allowlist has to be enforced where it is actually evaluated: at admission on the `ServiceConfiguration`, and again in the controller before any write. Narrowing the operator's identity is worth doing on its own merits, but it is a Milo-side change and this design must be correct without it.

#### The trust arrow

The most important structural property is that **the consumer pulls; the provider does not push.** A provider declares; nothing happens until a consumer creates an entitlement naming that service. A provider cannot enumerate or target projects. Every reconcile is keyed on an entitlement object a consumer created in their own plane, and the fanout runs only in the Active branch, after approval.

This property must survive every future extension. A proposal to let a provider select consumers — by label, by organization, by anything — is a redesign of the trust model, not a feature. It is worth noting that Milo's quota system already has the weaker shape: a grant-creation policy can target an arbitrary project chosen by a CEL expression and write into its plane with no consent from the receiving side. That is a precedent to avoid rather than to follow.

There is one genuine push, and it should be named: a provider that adds a resource to its declaration affects consumers who enabled earlier and never saw the new declaration — and additions to a Published configuration are always permitted. That is inherent to "the provider decides what its service needs," and it is why the bounds below concern *what* may be installed and not only *whether*.

#### What bounds quota grants today

Quota grants are the existing instance of exactly this trust, bounded by six things:

1. **The target kind is fixed by the platform** — a single quota kind, not a provider choice.
2. **The target namespace is fixed** — a platform namespace, not one the provider names.
3. **The name is derived** by hashing the canonical service name, the consumer project, and the limit name. A provider cannot choose a name, and therefore cannot collide with another provider's object or shadow one a consumer relies on.
4. **The content is an integer in a platform-defined schema**, referencing a metric that admission forces to carry the provider's own service-name prefix. Influencing another service's quota is not restricted so much as structurally impossible.
5. **Delivery requires an entitlement the consumer created**, and runs only after approval.
6. **Writes are labelled and pruned by selector**, so the controller only touches objects it created.

Bounds 1 through 4 exist because the provider supplies *values*, never *objects*. That is the whole trick, and it is what a generic mechanism has to preserve deliberately, since nothing outside the code preserves it.

It is also worth being honest that even here the bounds are content-side rather than identity-side, and one of them leaks: the quota consumer type is a provider-supplied API group and kind, so a provider can name another group in a declaration. Provider-supplied type references are the existing soft spot, and the allowlist below is the general fix for it.

#### What each option does to those bounds

Free-form inline manifests keep bounds 5 and 6 and destroy 1 through 4. Under that design, compromise of a single provider's configuration is equivalent to write access, as an omnipotent identity, into every plane of every project that enabled that service, with no restriction on kind — and therefore including kinds that grant access or cause execution.

Typed projection keeps all six. The kind is allowlisted by the platform; the name derives from the source object and the service; the content is a reference to an object the provider legitimately owns. Compromise of a provider's configuration yields allowlisted, non-privileged, name-scoped reference objects appearing in projects that enabled that service — confusing and disruptive, but not an escalation, and recoverable, because every object is labelled and owner-referenced so remediation is unpublishing the declaration plus a selector-scoped cleanup.

That contrast is the central argument of this document.

#### Provisioning must not launder authorization

The motivating case supplies a concrete and important instance of a general hazard, and it should shape the design rather than be discovered during implementation.

IPAM already authorizes cross-project class references. When a class naming another project's class is created, the API issues a subject access review for `use` on the source class, with the caller's parent scope rewritten to the source project so the question is meaningful. It fails closed. That is a well-built check.

But it is evaluated **against whoever creates the object**. If service-catalog creates the reference as the platform's `system:masters` identity, the check passes trivially and the consumer project never actually held `use` on the platform's class. The object then exists, and works, on the strength of the installer's authority rather than the consumer's. If the platform later intends to revoke that access, deleting the grant achieves nothing — IPAM checks at create, not at use, and a separate known gap means the allocation path follows the reference with no check at all.

The general rule this implies: **where the target API performs its own authorization, provisioning must establish the real grant rather than substitute the installer's authority for it.** For the motivating case that means the platform must ensure the entitled project genuinely holds `use` on the classes its entitled services declare — an IAM fact — and then create the reference in a way that check would independently accept.

That sits awkwardly beside the recommendation below that this mechanism must not install IAM bindings, and the tension resolves the same way as the RBAC case: the grant is **platform-authored, not provider-authored**. The provider declares which of its own classes it offers; the platform decides that an entitled project may use the classes of its entitled services, and constructs the binding — choosing the subject, the scope, and the verb. The provider never names a subject. That is a separate typed fanout with the same shape as quota, and it is a prerequisite for the motivating case being correct rather than merely working.

#### Controls

**A platform-owned allowlist of installable kinds.** For each allowed kind: whether it may be projected, whether it may be literal, which namespaces it may land in, which source field paths may be copied, and whether every provider may use it or only named ones. It must be a platform artifact — a resource only a platform admin can create, or static configuration in the platform repository — and never a field of a `ServiceConfiguration`. Enforced twice: at admission on the configuration, using the existing validating webhook machinery, so a provider gets a synchronous error rather than a silent status; and again in the controller before any write, so a bypassed or stale admission path cannot widen it. The operator's ClusterRole should still be kept honest and narrow, because it becomes a real ceiling the moment the `system:masters` identity is narrowed, and because it documents the intended surface.

**A denied set that is not negotiable.** Kinds that grant access or cause execution are excluded categorically rather than by omission: RBAC objects, IAM policy bindings, service accounts, secrets and token-bearing objects, admission webhook configurations, and custom resource definitions. These are the kinds whose installation converts "a provider can create an object" into "a provider can act as someone else, indefinitely, after the entitlement is gone."

This rules out one of the candidate use cases — per-project RBAC or IAM bindings — and that is the right outcome rather than an oversight. The need is real, and the enablement documentation already asserts that enabling a service provisions IAM roles even though no code does so. It should be met the way quota was: a **separate typed fanout** where the provider declares a role it is proven to own and the platform constructs the binding, choosing the subject, the scope, and the namespace. The provider says which of its own roles; the platform says who gets it and where. That keeps bounds 1 through 4 intact for the highest-risk case rather than routing it through a general installer — and it is the same construction the class-authorization problem above requires, which is a reason to build it early rather than defer it.

**Provider-scoped naming.** Every installed object's name derives from the canonical service name and the source object, or is required to carry the service prefix — the rule admission already enforces on metrics and monitored resource types. Two providers then cannot contend for one object, and a provider cannot shadow a name a consumer relies on.

**Source-project ownership proof.** In projection mode the source project must be the service's own producer project, or provably held by the same producer, enforced at admission. Without it a provider could project objects out of a project it does not own — and would do so with an identity that would not be stopped.

**Blast-radius bounds.** A cap on declared resources per configuration and on objects produced per project per service. When a selector matches more than the cap, refuse and report rather than truncate; a silent truncation is a correctness failure that looks like a working system.

**Attribution.** One identity writes into every plane, so an audit log shows the platform writing, not the provider whose declaration caused it. A per-service field manager improves attribution on the object; impersonating a per-service identity would improve it in the audit log and would also make the target API's own authorization checks meaningful rather than vacuous. The path exists in principle — Milo's project filter derives authorization scope from user extras, and the IPAM tests already simulate per-project identity by impersonation — but nothing in the operator does this today, and whether it is practical is an open question. It is the largest remaining gap in this model.

**First-party versus third-party.** All of the above is sized for providers who are first-party teams whose configurations go through platform review. The catalog's stated direction includes third-party providers. Under third-party providers the analysis changes qualitatively: the allowlist becomes load-bearing rather than defense-in-depth, and per-service identity becomes a requirement rather than an improvement. Which world this ships into should be decided before it ships.

### Failure legibility

The requirement is that a project which did not get its resources says so, in terms of the service that owed them.

**On the consumer side**, in the consumer's own plane, `ServiceEntitlement` gains a `Provisioned` condition alongside the existing `Ready`, plus a per-resource ledger in status: for each declared resource, what was expected, its state, and — when not installed — why, in language the consumer can act on. `Ready` continues to mean access and approval; `Provisioned` means delivery. Keeping them separate is what prevents a delivery failure from reading as a denial.

The compact-conditions-plus-downstream-detail pattern already exists in this repository: the configuration reconciler writes per-fanout health conditions and lets per-item status live on the produced objects, and it writes those conditions *before* returning the error. That is the model to copy. The entitlement reconciler currently does the opposite — dependency and quota failures return raw errors with no status surface, so an entitlement can read `Ready=True` while nothing was provisioned. Provisioning must not repeat that, and the existing fanouts should be brought up to the same standard, since a consumer cannot reasonably be expected to know which of three fanouts silently failed.

**On the provider side**, the same summary is mirrored onto the `ServiceConsumer` record, so a provider can see which consumers are short of what was declared without asking them.

**Rules for the message.** Name the service, name the resource, state the reason in terms the consumer can act on or escalate with. "The networking service could not install class `public-unicast` because this project's control plane does not serve that kind" is legible; a generic apply error is not.

Two failure modes must not be silent, because their silent forms have already been observed in the projection this generalizes:

- **The target kind is not served.** Report it; do not treat an absent kind as nothing to do.
- **Provisioning stops running at all.** A projection that quietly stops reconciling is indistinguishable from a project that legitimately has nothing, until an unrelated component reports a downstream symptom — which is exactly how the staging location outage surfaced, weeks late. The ledger should record when provisioning was last successfully evaluated, so "this has not been reconciled in a month" is visible on the object rather than inferable from someone else's incident.

Beyond conditions: events on the entitlement for each install and removal, and a metric of entitlements by provisioning state per service, so a stalled fanout is alertable rather than discoverable.

### Relationship to unconditional per-project seeding

Milo's project controller creates three per-project class objects at project creation — a gateway class, a DNS zone class, and a connector class — for every project, regardless of what it enables. Each is created bare: no owner references, no labels, no annotations, no finalizers. The helpers are get-then-create, so they are idempotent but do not correct drift; if someone edits one, nothing restores it. The only conditionality is whether the project's plane serves the kind at all. There is a standing TODO to remove one of them once project addons migrate to a new API, so the intent to move already exists.

Service-scoped provisioning differs in five ways that point the same direction.

- **Conditionality.** Unconditional seeding puts objects in projects that will never use them, so listing what a project has stops being a statement about what it uses.
- **Dependency direction.** Seeding encodes knowledge of specific services in the core project lifecycle: core Milo ends up depending on networking and DNS semantics, which is precisely the coupling the service catalog exists to remove. Provisioning inverts it — the service declares its own needs and core Milo knows only about entitlements.
- **Lifecycle.** Seeded objects have no teardown, no ownership, no versioning, no approval gate, and no failure reporting. Provisioned resources get all five from the entitlement. Note that the seeded objects are cluster-scoped and the project purger skips cluster-scoped resources, so nothing deletes them at project deletion either.
- **Drift.** Seeding never corrects an edited object; a `Managed` provisioned resource does.
- **Attribution.** A seeded object cannot answer "which service put you here and who owns you." A provisioned one can.

**Should they migrate? Yes, eventually — but not by weakening this mechanism.** The hazard is obvious: a project that has these objects today would lose them if it never enabled the corresponding service. The resolution is not to make provisioning unconditional; it is to make the *entitlement* unconditional for services every project is meant to have. Reframed that way, "seeded for every project" becomes "a service every project is entitled to by default" — a concept the catalog can express honestly, keeping one code path rather than two.

Migration then becomes: introduce default entitlements; adopt the existing objects with `Adopted` ownership, matched by name, so nothing is disturbed; and only then remove the seeding from the project controller. That sequencing is a precondition, not a follow-up — migrating before default entitlements exist would silently strip working projects.

## Prior art

**Cluster API's `ClusterResourceSet`** is the closest structural analogue: a cluster-scoped object selects target clusters and applies a set of manifests, either once or continuously. Two things are worth taking. First, the per-target binding object recording what was applied to which cluster and whether it succeeded — that is the per-consumer status ledger, and CAPI needed it for the same reason. Second, the apply strategy is an explicit enum rather than an implicit behavior. What does not fit is the targeting model: `ClusterResourceSet` selects targets by label, a push from an administrator with authority over both sides. Here targeting must be the consumer's own entitlement, because the two sides are different trust domains. Its content model — free-form manifests in config maps and secrets — is Option A, rejected above.

**The Kubernetes addon manager** contributes the reconcile-versus-ensure-exists distinction directly. It is the same choice as `Managed` versus `Adopted`, it has been in production for a very long time, and there is no reason to invent new vocabulary for it.

**OLM** contributes two ideas and a warning. The ideas: the permissions an installed component needs are part of the reviewable artifact rather than something the installer decides — which is exactly the discipline the current design lacks, since nothing in a `ServiceConfiguration` today states what it will cause to be written; and a change can require explicit approval before landing on an existing installation, a direct answer to the rollout question left open above. The warning is OLM's dependency resolver and registry model, widely regarded as its principal failure, and something this design has no need for: `Service.spec.dependencies` is a simpler, sufficient model, and provisioned resources do not resolve against each other.

**Crossplane compositions** map cleanly — a claim rendering a set of managed resources is the same shape as an entitlement rendering a set of provisioned ones — and supply the most useful cautionary tale here. Patches-and-transforms started as a closed substitution set and grew, case by defensible case, until composition functions running arbitrary code became necessary. That is the trajectory the templating section is designed to refuse. What does not fit is provider-authored arbitrary resource graphs: a service declaring what a consumer needs is a much smaller problem than composing infrastructure.

**Flux and Argo multi-tenancy** contribute the most directly applicable result, and it is a negative one. Both began with a single controller applying whatever a tenant's source said, with cluster-wide authority — which is precisely the position this platform is in today, with a `system:masters` identity reaching every project plane. Both had to add the same two things: impersonation of a per-tenant identity, and an allowlist of permitted destinations and kinds. Argo's project object, which allowlists destinations and permitted cluster-scoped and namespace-scoped resources, is functionally the platform-owned kind allowlist proposed here, arrived at independently from the same problem. The lesson to take is that the allowlist and the write identity are not refinements to add later; they are what makes the mechanism correct at all, and retrofitting them is much harder than starting with them. The lesson not to take is git as the declaration source, which is Option B.

**What does not apply.** Helm-style values and templating assume a user supplying configuration at install time; there is no such user here, and inventing one would hand consumers a knob on platform-owned objects. Anything assuming an agent inside the target cluster does not apply either — project planes are virtual, reached by path rewrite, with no per-project agent to delegate to. And package-manager semantics generally — versioned upgrades, rollbacks, inter-package conflict resolution — are far more machinery than "a service says what a project needs" requires, and would import the failure modes without the benefits.

## Recommendation

**Add a `spec.provisioning` section to `ServiceConfiguration` whose primary mode is a typed projection of provider-owned objects into entitled projects; deliver it by generalizing the existing location-binding reconciler; bound it with a platform-owned allowlist of installable kinds enforced at admission and in the controller; gate it on the entitlement being Active; own installed objects via owner references to the entitlement with label-scoped pruning as the fallback; and report through a `Provisioned` condition with a per-resource ledger on both the consumer's entitlement and the provider's consumer record. Pair it with a separate typed IAM fanout so that provisioning establishes real authorization rather than substituting the installer's.**

The reasoning in one line: every existing fanout works by having the provider supply values into a platform-defined schema, and that is what makes the current trust boundary tractable given an omnipotent write identity; a projection preserves that property while giving the motivating case everything it needs, and free-form manifests do not.

**Alternatives and why they lost.**

- *Inline free-form manifests as the primary mode* — fastest and most expressive, but it discards four of the six bounds that make the existing instance of this trust safe, and there is no RBAC ceiling behind it to catch the difference. Admitted later only in a bounded, allowlisted, literal form for resources that are not projections of anything.
- *An OCI or git bundle reference* — moves authoritative content outside the API that governs it, compounding an already weak versioning story; adds a supply chain and a provenance problem; worsens failure legibility; and grants strictly more trust than inline manifests while costing more. Revisit only if provisioned content outgrows an API object.
- *A fourth bespoke controller for IP classes* — smallest change, solves today's problem, and it is the honest benchmark: if the generic mechanism costs much more than one more hand-written reconciler, it is over-designed. It loses because it is the fourth instance of the same pattern, leaves the next case where this one started, and does nothing about the declaration gap the engagement enhancement already identified as blocking access scoping.
- *Extending Milo's unconditional project seeding* — no approval gate, no teardown, no attribution, no drift correction, and it deepens core Milo's dependency on specific services. It also cannot express "compute needs this because it depends on networking," which is the actual requirement.
- *Holding the entitlement out of Active until provisioning succeeds* — makes a broken project obviously broken, but conflates access with delivery and makes a transient apply failure indistinguishable from a provider denial.
- *Treating the operator ClusterRole as the ceiling* — attractive because it needs no new machinery, but it describes a control that is not in force against a `system:masters` identity. Rejected as unsound rather than insufficient.

### What to build first

The proof should be falsifiable, and the falsification test is whether the generic mechanism can express what the bespoke one already does.

1. **The allowlist and the delivery path**, with exactly one allowlisted kind — the IPAM class in source-reference form. `Managed` ownership, projection mode only, no literal mode, no field copying, no templating beyond the closed value set. The motivating case needs no new API kind, which makes this genuinely small.
2. **The authorization pairing**: the entitled project actually holds `use` on the platform classes its entitled services declare, established by the platform rather than asserted by the installer, so the reference would survive the target API's own check. Without this the first milestone works but is not correct.
3. **The IPAM case end to end**: one platform project holding the classes, a networking configuration declaring the projection, a consumer project that enables networking and can allocate, and a consumer project that enables compute and inherits through the dependency.
4. **The status ledger** — `Provisioned` on the entitlement, mirrored to `ServiceConsumer`, including the kind-not-served and last-evaluated cases. This is not a follow-up; it is half the value.
5. **Migrate the location projection onto it** and delete the bespoke reconciler. If locations cannot be expressed, the model is wrong and should be revised before anything is built on it.

Only then the literal mode, and only with a concrete second use case in hand rather than speculatively.

### Open questions needing a human decision

1. **Who owns the allowlist, and how does a kind get added?** A platform-admin-created resource, or static configuration in the platform repository? The second is more auditable; the first is more operable.
2. **Does a declaration change roll out to existing consumers immediately, or require acceptance?** Immediate matches today's behavior, and additions to a Published configuration are already unrestricted. Pinning or staged acceptance is safer and more machinery.
3. **Should the operator get a per-service write identity?** It would make audit attribution real and, more importantly, make target APIs' own authorization checks meaningful instead of vacuous. This is the largest gap in the security model and it is a Milo-side change.
4. **Should disabling a service be blocked while installed resources are in use**, or should the delete be issued and the owning API refuse to leave dangling state? This proposal recommends the latter; it is a product decision as much as a technical one.
5. **Should default-for-every-project entitlements exist?** This is the clean migration path for the existing unconditional seeding, and the catalog has no such concept today.
6. **First-party providers only, or third-party?** The posture differs qualitatively, and the answer decides which controls are defense-in-depth and which are load-bearing.
7. **Which Published configuration is active?** The two existing fanouts disagree. A single rule should be chosen and applied to all three.
8. **Who fixes cluster-scoped cleanup at project deletion?** The purger omits cluster-scoped resources; both existing and proposed provisioned objects are cluster-scoped. This is a Milo bug that provisioning amplifies.

### What could not be determined

- Whether the operator's identity can practically be narrowed from `system:masters` to something per-service or per-project. The server-side plumbing derives authorization scope from user extras and the IPAM tests simulate per-project identity by impersonation, so the shape exists; whether the operator can adopt it without breaking the paths it depends on was not established.
- Whether a deleted project's storage keyspace is reclaimed in the shared-apiserver model. No code performing that reclamation was found, which bears directly on whether cluster-scoped installed objects are eventually collected or merely orphaned.
- Whether the IPAM allocation path's missing authorization check on followed references is intended to be closed before or after a mechanism like this starts creating those references at scale. It is tracked upstream, and the ordering matters.
- Whether any bound exists today, other than controller code, on what a provider's declaration can cause to be written into a consumer plane. Nothing found suggests one does.

## Production Readiness Review Questionnaire

### Feature Enablement and Rollback

#### How can this feature be enabled / disabled in a live cluster?

- [x] Other
  - Describe the mechanism: provisioning occurs only for services whose `ServiceConfiguration` declares `spec.provisioning`, and only for kinds present in the platform allowlist. Emptying the allowlist disables it globally; removing the section disables it for one service.
  - Will enabling / disabling the feature require downtime of the control plane? No.
  - Will enabling / disabling the feature require downtime or reprovisioning of a node? No.

#### Does enabling the feature change any default behavior?

Not until a provider declares provisioning. Once declared, entitled projects gain objects they did not previously have.

#### Can the feature be disabled once it has been enabled?

Yes. Removing the declaration prunes what it installed, by the path that already handles service disablement. The consequence is that projects lose resources they may now depend on — the same hazard as disabling the service.

#### What happens if we reenable the feature if it was previously rolled back?

Resources are reinstalled on the next reconcile. Projection mode is derived rather than authored, so reinstallation is idempotent.

#### Are there any tests for feature enablement/disablement?

To be added: declaring and undeclaring provisioning on a Published configuration, asserting install and prune in a consumer project, and asserting that a gated entitlement provisions nothing before approval.

### Rollout, Upgrade and Rollback Planning

#### How can a rollout or rollback fail? Can it impact already running workloads?

The primary risk is a declaration change reaching every consumer at once, since the configuration is live and additions to a Published document are permitted. A bad declaration could install wrong references or prune correct ones platform-wide within one resync; workloads depending on a pruned class would fail to allocate.

#### What specific metrics should inform a rollback?

Entitlements in a non-provisioned state per service; prune rate; age of the oldest entitlement not successfully evaluated.

#### Were upgrade and rollback tested?

Not yet.

#### Is the rollout accompanied by any deprecations?

Yes, eventually: the bespoke location-binding reconciler is intended to be removed once locations are expressed through this mechanism, and the project controller's unconditional class seeding once default entitlements exist.

### Monitoring Requirements

#### How can an operator determine if the feature is in use by workloads?

Count of configurations declaring provisioning, and of entitlements with a non-empty provisioned-resource ledger.

#### How can someone using this feature know that it is working for their instance?

- [x] API .status
  - Condition name: `Provisioned` on `ServiceEntitlement`, mirrored on `ServiceConsumer`
  - Other field: the per-resource ledger in `ServiceEntitlement.status`
- [x] Events
  - Event Reason: resource installed, resource removed, resource unprovisionable

#### What are the reasonable SLOs?

Resources present in an entitled project within one resync interval of the entitlement becoming Active, for the large majority of activations; no entitlement left un-evaluated for longer than a small multiple of the resync interval.

#### What are the SLIs an operator can use?

- [x] Metrics
  - Entitlements by provisioning state and service; age of the oldest un-evaluated entitlement; apply error rate by kind and reason.

#### Are there any missing metrics?

Cross-plane reconcile latency is not instrumented anywhere in this repository today, and it is the quantity that matters most here.

### Dependencies

Project control-plane availability: an unreachable plane cannot be provisioned, and the entitlement must say so rather than appear settled. Root-plane availability for reading configurations and source objects. The IAM system, for the authorization pairing described above.

### Scalability

#### Will enabling / using this feature result in any new API calls?

Yes: per entitlement per resync, a read of the active configuration and the source objects, plus an apply per declared resource. The dominant cost is the periodic resync across every engaged project cluster, which already exists for location projection — and note the operator currently engages every Ready project, not only entitled ones.

#### Will enabling / using this feature result in introducing new API types?

One cluster-scoped platform type for the kind allowlist, plus new fields on `ServiceConfiguration` and `ServiceEntitlement`.

#### Will enabling / using this feature result in increasing size or count of the existing API objects?

`ServiceConfiguration` grows by a bounded declaration; `ServiceEntitlement.status` by a bounded per-resource ledger; consumer projects by a bounded number of objects per entitled service.

#### Will enabling / using this feature result in increasing time taken by any operations covered by existing SLIs/SLOs?

Entitlement activation gains a provisioning step. Because `Ready` and `Provisioned` are separate conditions, activation latency is not gated on it.

#### Can enabling / using this feature result in resource exhaustion?

Bounded by the per-configuration and per-project caps in the security model. Without them a broad selector over a large source project fans out multiplicatively across every entitled project, which is why the caps are part of the design rather than an operational concern.

### Troubleshooting

#### How does this feature react if the API server is unavailable?

Reconciles fail and retry; installed resources are unaffected. The ledger retains the last successful evaluation time, making an outage visible rather than silent.

#### What are other known failure modes?

- Target kind not served in a project plane — detected via the unprovisionable reason; mitigated by fixing the plane's API surface or removing the declaration.
- Selector matches more than the cap — an explicit refusal reason, deliberately not a truncation.
- Provisioning stalls entirely — detected via the age of the oldest un-evaluated entitlement, the signal that was missing when location projection stalled in staging.
- Source project unreachable or source objects unshared — reported per resource rather than aggregated into a generic failure.
- Installed objects surviving project deletion — cluster-scoped resources are not purged; tracked as a Milo-side gap.

#### What steps should be taken if SLOs are not being met?

Check whether the fanout is running at all before investigating individual entitlements. The stall mode and the per-resource failure mode have very different remedies, and only the second is visible on a single object.

## Implementation History

- 2026-08-17: initial draft.

## Drawbacks

The strongest argument against building this is that one more bespoke controller solves the motivating case sooner, and the platform has three instances of the pattern so far — arguably not yet enough evidence to generalize. If the generic mechanism costs substantially more than a fourth hand-written reconciler, it is over-designed and should be cut back until it does not.

The second is that this creates a capability whose safe form is narrow and whose unsafe form is a small edit away, on a foundation with no enforcement behind the API. Every future request — "can I just install one manifest," "can I template one more field," "can I target consumers by label" — moves it toward the unsafe form, and each will be individually reasonable. A mechanism that requires ongoing discipline to stay safe is worse than one that is safe structurally, and this one requires discipline until the write identity is narrowed.

The third is sequencing: the authorization pairing is a prerequisite for the motivating case to be correct rather than merely functional, and it is work in a different system. Shipping the delivery path without it produces something that demonstrably works and is quietly wrong, which is the worst outcome available.

## Alternatives

Covered inline: [declaration options](#declaration-how-a-provider-expresses-what-to-install) compares inline manifests, external bundles, and typed projection; [Recommendation](#recommendation) records why each losing option lost, including a fourth bespoke controller, extending unconditional project seeding, and relying on the operator ClusterRole as a ceiling.
