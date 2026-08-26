---
status: implementable
stage: stable
latest-milestone: "v0.3"
---

# Service Enablement — CLI Experience

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
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)

## Summary

Whoever is at the terminal should have one honest, consistent way to see what Datum services are available to their project, ask for access to one, and check in on a request — the same three commands, the same copy, the same behavior, no matter which service it's about or which Datum command-line tool they're using. And someone who hits a gated feature for the first time — running `datumctl compute instances` before they've ever heard the word "enablement" — should be able to enroll right there, in that same command, instead of being redirected somewhere else first.

This proposes a shared capability and command group (`services list` / `enable` / `status`) that deliver that experience, hosted next to the [`ServiceEntitlement`](../service-enablement.md) API itself so any Datum CLI can adopt it directly rather than building its own version.

## Motivation

[Service enablement](../service-enablement.md) gives every project a real, trustworthy signal for whether it's using a service, and why: pending, active, denied, revoked, unavailable. The platform doesn't yet give consumers an equally trustworthy way to see that signal and act on it — there's no single, dependable place to discover what's available, ask for access, and get an honest answer about where a request stands. Left unaddressed, every consumer-facing surface has to solve that on its own.

That's a correctness problem, not just a convenience one. The meaning of an entitlement's state belongs to the enablement model itself, not to whichever surface happens to be showing it — so every place that re-derives that meaning independently is a place it can be gotten wrong, or drift out of date as that meaning evolves. This proposes closing that gap once, as a capability the platform provides directly.

### Goals

- Give every Datum CLI plugin the same, correct, current understanding of what an entitlement's state means, from one place, instead of N independent and drifting copies.
- Give a consumer a single, memorable command surface — `datumctl services list/enable/status` — that works identically for any service on the platform.
- Give scripts real signal: distinct, documented exit codes for "not enabled," "denied or revoked," "pending," and "unavailable," instead of one catch-all failure.
- Let a plugin auto-enroll a first-time user inline, as a side effect of the command they actually ran, instead of requiring a separate command first.
- Keep the interpretation of entitlement state owned by the same team that owns the API producing that state, so a meaning change and its user-facing update happen together, not separately and out of sync.

### Non-Goals

- This is not a new access-control mechanism. It's a client and a command surface over the `ServiceEntitlement`/`ServiceConsumer` model [Service Enablement](../service-enablement.md) already defines — it doesn't change who can access what, only how a consumer finds out and asks.
- This is not specific to `datumctl` or to compute. The compute plugin is the intended first adopter, but nothing about the design assumes that caller.
- Provider-side tooling (a symmetric CLI for providers managing their `ServiceConsumer` records — seeing who's requested access, approving or denying) is out of scope here; see [Alternatives](#alternatives) and follow-on work.

## Proposal

Provide this as a single, shared capability — built and maintained alongside the `Service` and `ServiceEntitlement` APIs it interprets — that any Datum command-line tool can adopt directly, instead of every tool recreating it on its own.

It's meant to be used two ways:

1. **A standalone command group** (`services list`/`enable`/`status`) that a host CLI adds alongside its other commands, giving consumers an explicit way to browse and manage enablement across every service on the platform.
2. **An automatic check** a plugin runs before its own commands, so a consumer who hasn't enabled a service yet gets prompted and enrolled inline, in the same invocation, rather than being told to go run a different command first.

Both draw on the same understanding of what an entitlement's state means, so the two never disagree about what to tell the user.

### User Stories

#### Story 1: A consumer explores what's available

```
$ datumctl services list
NAME       STATE              SINCE
compute    Active             enabled 3d ago
ipam       Not requested
storage    Pending approval   requested 2h ago
```

#### Story 2: A consumer requests access to a new service

```
$ datumctl services enable compute
Compute is not enabled for project "datum-cloud".
Enabling it needs approval from the team that provides compute.
Would you like to request access? [y/N]: y
Requesting access to compute for project "datum-cloud"...
⠋ Waiting for the platform to process the request... (4s)

Your request to enable compute for project "datum-cloud" has been submitted.

  Status:  Pending approval — Waiting for the service provider to approve this request.
           Someone reviews this by hand, so it can take a while.

Check progress with:   datumctl services status compute
Wait for activation:   datumctl services enable compute --wait
```

Checking in later is the same command regardless of which service it's about:

```
$ datumctl services status compute
Service:  Compute (compute.datumapis.com)
Project:  datum-cloud
Status:   Pending approval (requested 2h ago)
          Waiting for the service provider to approve this request.
```

#### Story 3: A first-time user auto-enrolls from a plugin's own command

Nobody wants to be told to go run a different command before they can do the thing they came to do. So `datumctl compute instances` — compute's own command, not the `services` tree — checks enablement first, enables the service, and continues into the command the user actually asked for:

```
$ datumctl compute instances
Compute is not enabled for project "datum-cloud".
...
Compute is now enabled for project "datum-cloud".

NAME       STATUS    CREATED
web-01     Running   3d ago
```

Whether it asks first is decided by the service's enablement mode, not by whether a terminal happens to be attached:

- **Self-service.** The request cannot be refused — it enables the service there and then — so there is nothing to confirm. It is enabled without a prompt, on a TTY or not, and the command carries on. Enabling is announced on stderr so it is never silent.
- **Gated by provider.** The request goes to a human and can sit unapproved, so it is worth confirming. On a TTY it walks through the same request shown in Story 2. Without one it neither prompts nor blocks: it reports the state plainly and exits with the same documented code `datumctl services status` would show.

Keying on the mode rather than the terminal is what makes a self-service command usable from CI, where there is nobody to answer a prompt and refusing is the only other option.

### Notes/Constraints/Caveats

- **Hosting decision.** This capability would live here, not inside any one CLI, so the team that changes what a phase or condition reason means is the same team that updates the experience built on it. A precedent already exists for this shape of arrangement: `datumctl` already adopts its activity-log capability from a separate, shared repository rather than reimplementing it — hosting something once and adopting it into a host CLI is an established, low-risk pattern here, not a new one.
- **Two API surfaces, one command.** The platform's [catalog of registered services](../service-registry.md) and a project's own enablement records live in different places — the catalog is platform-wide, enablement is project-scoped. A command like `services list` needs to reach both and bring them together itself; nothing joins them automatically today. A host CLI supplies a way to reach each; this capability handles the rest.
- **Adoption should be simple, not a rewrite.** A host CLI provides where its output should go and a way to reach those two API surfaces, and gets the full experience — prompting, waiting, rendering, exit codes — for free. Nothing about the design assumes a particular caller.

### Risks and Mitigations

- **Risk:** this repo's user-facing copy drifts from what a specific host CLI's users expect, since it's no longer each CLI's own to adjust freely. **Mitigation:** the first adopter (`datumctl`) reviews this copy as part of adopting it, the same as it would for any other command it ships; copy changes land here, visible to every adopter at once, rather than diverging per plugin.
- **Risk:** auto-enrollment (Story 3) makes enabling a service — which, per [Service Enablement](../service-enablement.md), triggers real billing and quota provisioning — a side effect of a command the consumer ran for another reason. For a self-service service there is no confirmation at all. **Mitigation:** the confirmation was never much of a control here, because the consumer can enable the same service unilaterally with one command; withholding it only meant the consumer typed that command themselves. What the mitigation rests on instead is that enabling is always announced, naming the service and the project, so it is visible rather than silent; that it is idempotent, so it happens once per project rather than once per command; and that a service whose enablement genuinely needs a gate is marked `GatedByProvider`, which still requires an explicit yes and never enrolls without one. A provider that wants enablement to be a deliberate act has a supported way to say so, rather than relying on every CLI to prompt.

## Design Details

An entitlement (or its absence) is reduced to one of a small set of states — not requested, processing, pending approval, active, denied, revoked, unavailable, or catalog-unavailable — using only the information the enablement API already exposes: its phase, its readiness, and when it was first granted.

The standalone command group and the automatic check used for auto-enrollment (Story 3) share that same classification: the automatic check simply applies it before a plugin's own command runs, rather than as its own command, and a plugin decides which of its commands it applies to.

Exit codes are stable and documented per state (not enabled, denied-or-revoked, pending, unavailable), distinct from a general failure, so a script can tell what actually happened instead of treating every non-zero result the same.

## Production Readiness Review Questionnaire

This is a client-side capability with no control-plane component, feature gate, or cluster rollout of its own — a consuming CLI adopts or rolls back a version the same as any other dependency, and doing so changes no existing API or controller behavior. Most of the standard questionnaire (feature gates, cluster/node rollback, scalability of new API types, resource exhaustion) assumes a control-plane feature and doesn't apply. The one operational note worth capturing: if the platform API is unreachable, both the standalone commands and the automatic check fail loudly and exit non-zero, rather than hanging or silently treating a service as unusable.

## Implementation History

- 2026-07-13 — opened as a three-part stack: the shared entitlement-interpretation and request logic (milo-os/service-catalog#51), the `services list`/`enable`/`status` command group (#52), and this doc (#53).
- Pending — `datumctl` and the compute plugin adopting it (Story 3); `ipam` migrating its own hand-rolled version of this flow onto it.

## Drawbacks

- This repo takes on user-facing copy and UX review as an ongoing responsibility, not just API/controller design — a different kind of burden than the rest of this repo carries today.
- A consuming CLI takes on a dependency on this repo's release cadence for user-facing fixes, rather than patching its own copy immediately (see [Notes/Constraints/Caveats](#notesconstraintscaveats) for why this trade-off was accepted).

## Alternatives

- **Keep this logic inside `datumctl`, generalized in place.** This is what the original draft did. Rejected because it doesn't fix the actual problem: the interpretation of entitlement state would still live away from the API defining what it means, and a second CLI wanting the same experience would have to depend on `datumctl` itself.
- **Provide only shared access to entitlement data here, and leave interpreting it and building the experience to `datumctl`.** Reduces but doesn't eliminate drift — a change to what a phase or reason means would still require a second, separate update in `datumctl`'s own copy of that interpretation.
- **Leave every plugin to hand-roll its own flow**, i.e. the status quo. This is the actual problem being solved, not a real alternative.

## Infrastructure Needed

None. This uses the repo's existing release and CI infrastructure; the only change is a new command-line-tooling dependency for the command-line piece of this work.
