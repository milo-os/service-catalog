---
status: provisional
stage: alpha
latest-milestone: "v0.x"
---

# Platform feature flags

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [PlatformFeatureFlag resource](#platformfeatureflag-resource)
  - [Access control](#access-control)
  - [Evaluation](#evaluation)
  - [Audit and change history](#audit-and-change-history)
  - [Relationship to other mechanisms](#relationship-to-other-mechanisms)
  - [Client integration (informative)](#client-integration-informative)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
  - [Feature Enablement and Rollback](#feature-enablement-and-rollback)
  - [Rollout, Upgrade and Rollback Planning](#rollout-upgrade-and-rollback-planning)
  - [Monitoring Requirements](#monitoring-requirements)
  - [Dependencies](#dependencies)
  - [Scalability](#scalability)
  - [Troubleshooting](#troubleshooting)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [References](#references)

## Summary

Platform operators have no single runtime switch to turn product behavior on or off everywhere. Today that behavior is split across org-scoped quota Feature buckets, per-project service entitlements, and client-side environment variables. None of those lets an operator flip one behavior for every organization, project, and client at once.

This enhancement adds platform feature flags: a catalog of named, platform-wide on/off switches the platform team controls at runtime. An operator can turn a behavior on or off in seconds, with no redeploy and no per-organization changes, and every change is recorded on the activity timeline showing who changed what and why. Clients evaluate flags fail-closed, so if the platform can't be reached the gated behavior stays off rather than guessing.

The first flag is auto-create traffic protection on tunnel and edge create (default on), shared by the Datum desktop app and the cloud portal.

Sibling catalog docs: [`Service`](./service-registry.md), [`MeterDefinition`](./metering-definitions.md), [`MonitoredResourceType`](./monitored-resource-types.md). Enhancement template: [datum-cloud/enhancements/template](https://github.com/datum-cloud/enhancements/tree/main/enhancements/template).

## Motivation

Some product behavior should default on for everyone: auto-provisioning a default WAF when a tunnel is created, shipping a new UI path, or pausing a rollout during an incident. The platform team needs to change that behavior at runtime without redeploying every client or touching quota on every organization.

| Mechanism | Scope | What it answers |
|-----------|-------|-----------------|
| [Service entitlement](./service-enablement.md) | Project | Has this project opted into this managed service? |
| Organization feature entitlement | Organization | Does this org get a commerce- or tier-gated feature? |
| Client build / environment settings | Install / release | Is this client configured to do X? |

Auto-create WAF on tunnel create shows the gap. It is not "this project enabled networking." It is platform-wide behavior built into the create flows of the Datum desktop app and the cloud portal.

Organization feature entitlements work well for billing-gated UI, but driving a global rollout that way means changing entitlements org by org. Client environment settings are not shared between the desktop app and the portal, and can't be flipped mid-incident without a release.

### Goals

- One named, platform-wide switch per rollout or kill-switch behavior, owned by the platform team.
- Operators turn a behavior on or off once, with the same result for every organization, project, and user, and without a redeploy or per-org changes.
- Behavior stays off when the platform can't confirm a flag is on (fail-closed), so outages can't silently turn risky defaults on.
- A clear audit trail of every change, surfaced on the activity timeline: who flipped a flag, when, and why.
- First consumer: auto-create traffic protection on tunnel / edge create, shared by the Datum desktop app and cloud portal.

### Non-Goals

- Replacing org-scoped Feature `AllowanceBucket` flags (billing / tier UI stays on quota).
- Per-project service enablement ([`ServiceEntitlement`](./service-enablement.md)).
- Arbitrary config payloads (v0 is boolean only).
- Percentage or cohort rollouts by org or project.
- Runtime service discovery (endpoints, health, routing).

## Proposal

Add platform feature flags as a new catalog primitive: named, platform-wide switches that the platform team registers once and controls at runtime. An operator registers a flag, turns it on or off, and clients pick up the change within a few seconds. Each flag is a simple boolean today, on or off for everyone, with room to grow later. The concrete API shape, evaluation rules, and audit wiring live in [Design Details](#design-details).

### User Stories

#### Platform operator disables auto-create WAF during an incident

An operator sees false positives from default traffic protection rules. From the platform operator UI (or the command line), they turn the auto-create traffic protection flag off and record why. Within a few seconds, the Datum desktop app stops auto-creating traffic protection on tunnel create and the cloud portal stops defaulting WAF enforcement to on. The activity timeline shows who disabled the flag and when.

#### Platform operator re-enables after fix

After deploying updated rules, the operator turns the flag back on. New tunnels and edge creates resume auto-provisioning. The flag's own change history records the flip, so operators don't need a separate audit query.

#### Client evaluates flag fail-closed

The Datum desktop app checks platform flags at session start. If Milo is unreachable, auto-create traffic protection stays off rather than assuming on from a stale or local value.

### Notes/Constraints/Caveats

- A flag's initial value sets where it starts when first registered. It is not a client-side fallback when the platform is unreachable; clients still fail closed.
- During rollout, the Datum desktop app may honor an existing local override until the platform flag is everywhere. When both exist, an explicit operator "off" at platform scope wins.
- Today's org portal feature flags are read-only evaluation with no audit timeline and no operator toggle. Platform feature flags are a separate primitive for platform-wide operator control.

<<[UNRESOLVED]>>
- Retired flags: remain readable forever in list/get APIs, or hidden from discovery while preserved in etcd?
- Change reason required on disable only, on every disable, or never?
- On-object change history cap (64 suggested): large enough for incident windows, small enough to keep the record bounded?
- Cache TTL (5s suggested): tradeoff between incident response speed and API load.
<<[/UNRESOLVED]>>

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Operator disables flag; clients with long cache TTL keep old behavior | Short TTL (~5s); optional control-plane admission rejects CREATE when flag is off |
| API outage causes widespread behavior change (fail-closed) | Documented contract; auto-create WAF is safe to skip; operators monitor flag fetch errors |
| `status.history` grows unbounded | Cap at 64 entries; full record remains in apiserver audit |
| Confusion with org Feature AllowanceBuckets | Separate resource, API group placement, and doc table comparing scope |

## Design Details

### PlatformFeatureFlag resource

```yaml
apiVersion: services.miloapis.com/v1alpha1
kind: PlatformFeatureFlag
metadata:
  name: auto-create-traffic-protection
spec:
  key: networking.datumapis.com/auto-create-traffic-protection
  displayName: Auto-create traffic protection on tunnel / edge create
  description: When enabled, clients auto-provision a default TrafficProtectionPolicy on create.
  enabled: true          # operator toggle; the field operators PATCH
  owner:
    serviceRef:
      name: networking.datumapis.com
  phase: Active
status:
  enabled: true          # controller mirrors spec.enabled; clients read this
  lastTransition:
    enabled: true
    changedAt: "2026-06-19T14:22:00Z"
    changedBy:
      username: platform-op@datum.net
      uid: "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
    reason: "INC-4821: disable auto-create WAF during rule rollout"
    source: portal
  history:
    - enabled: false
      changedAt: "2026-06-18T09:00:00Z"
      changedBy:
        username: platform-op@datum.net
        uid: "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
      reason: "Pre-release validation"
      source: kubectl
    - enabled: true
      changedAt: "2026-06-19T14:22:00Z"
      changedBy:
        username: platform-op@datum.net
        uid: "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
      reason: "INC-4821 resolved"
      source: portal
  conditions: []
```

| Field | Value |
|-------|-------|
| API group | `services.miloapis.com/v1alpha1` |
| Kind | `PlatformFeatureFlag` |
| Scope | Cluster |
| Parent context | `Platform` (root etcd) |
| Discovery | `discovery.miloapis.com/parent-contexts=Platform` |

Two names, deliberately: `metadata.name` is the Kubernetes slug; `spec.key` is the reverse-DNS identifier clients pass to evaluators (same convention as [`Service`](./service-registry.md)).

Lifecycle in `spec.phase`: `Draft` → `Active` → `Retired`. Evaluators treat `Draft` and `Retired` as off. Optional `spec.owner.serviceRef` links to a registered `Service` for display and audit lineage only.

Operators write `spec.enabled`. The controller reconciles it into `status.enabled` and records each change in `status.history`. Clients read `status.enabled`, the reconciled value, so the live state always reflects what the controller has accepted.

### Access control

Access is modeled with Milo IAM (`iam.miloapis.com/v1alpha1`), the same way the rest of this service handles RBAC. A `ProtectedResource` declares the permissions for `PlatformFeatureFlag` (Platform parent context), and `Role` objects bundle them.

```yaml
apiVersion: iam.miloapis.com/v1alpha1
kind: ProtectedResource
metadata:
  name: services.miloapis.com-platformfeatureflag
spec:
  serviceRef:
    name: services.miloapis.com
  kind: PlatformFeatureFlag
  plural: platformfeatureflags
  singular: platformfeatureflag
  permissions:
    - list
    - get
    - watch
    - create
    - update
    - patch
    - delete
    - updateStatus
```

Operators toggle a flag by writing `spec.enabled`. The controller owns `status`: it reconciles `spec.enabled` into `status.enabled` and appends `status.history`, so operators never write `status` directly (only the controller holds `updateStatus`). Service owners get no write access through `spec.owner.serviceRef`.

Flipping a flag is high blast radius, so v0 adds a dedicated `services.miloapis.com-feature-flag-admin` role kept separate from full services admin. Platform operators can hold it on its own. Matching the existing `-approver` and `-entitlement-admin` precedent, `services.miloapis.com-admin` inherits it, and read access (`get`, `list`, `watch`) folds into `services.miloapis.com-viewer`. The portal operator UI and break-glass kubectl bind to the same role.

```yaml
apiVersion: iam.miloapis.com/v1alpha1
kind: Role
metadata:
  name: services.miloapis.com-feature-flag-admin
  annotations:
    kubernetes.io/display-name: Feature Flag Admin
    kubernetes.io/description: "Register and toggle platform feature flags"
spec:
  launchStage: Alpha
  includedPermissions:
    - services.miloapis.com/platformfeatureflags.get
    - services.miloapis.com/platformfeatureflags.list
    - services.miloapis.com/platformfeatureflags.watch
    - services.miloapis.com/platformfeatureflags.create
    - services.miloapis.com/platformfeatureflags.update
    - services.miloapis.com/platformfeatureflags.patch
    - services.miloapis.com/platformfeatureflags.delete
```

### Evaluation

Clients and optional control-plane admission share one rule. Evaluation is fail-closed.

1. Resolve the flag by `spec.key` (GET or cached list).
2. If lookup succeeds and the object is `Active`, use `status.enabled`.
3. If lookup succeeds but the object is missing, not `Active`, or `status.enabled` is false, treat as off.
4. If lookup fails (API error, timeout, auth failure, or no successful fetch yet), treat as off.

No `targetingKey`. No org, project, or user dimension.

```mermaid
flowchart LR
    subgraph eval [Evaluation]
        Key["spec.key lookup"]
        Fail["Lookup failed → off"]
        Active["Active and enabled?"]
        Off["Otherwise → off"]
        On["on"]
        Key --> Fail
        Key --> Active
        Active -->|yes| On
        Active -->|no| Off
    end
    subgraph clients [Consumers]
        Portal["Cloud portal"]
        Desktop["Datum desktop app"]
        Admission["Optional control-plane admission"]
    end
    On --> Portal
    On --> Desktop
    On --> Admission
    Off --> Portal
    Off --> Desktop
    Off --> Admission
    Fail --> Portal
    Fail --> Desktop
    Fail --> Admission
```

### Audit and change history

Flipping a platform flag changes behavior for every org, project, and client. Audit is part of v0.

Three layers:

1. **Kubernetes apiserver audit.** Every PATCH is recorded (actor, verb, body, timestamp). Input to Milo activity.
2. **On-object history.** Controller appends to `status.history` and updates `status.lastTransition`. Only the controller writes these fields.
3. **ActivityPolicy summaries.** `auditRules` match PATCH events and produce human-readable timeline entries (billing pattern). The controller does not emit separate timeline events.

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityPolicy
metadata:
  name: services.miloapis.com-platformfeatureflag
spec:
  resource:
    apiGroup: services.miloapis.com
    kind: PlatformFeatureFlag
  auditRules:
    - name: enable
      match: "!audit.user.username.startsWith('system:') && audit.verb in ['update', 'patch'] && has(audit.requestObject.spec) && audit.requestObject.spec.enabled == true"
      summary: "{{ actor }} enabled platform feature {{ audit.requestObject.spec.key }}"
    - name: disable
      match: "!audit.user.username.startsWith('system:') && audit.verb in ['update', 'patch'] && has(audit.requestObject.spec) && audit.requestObject.spec.enabled == false"
      summary: "{{ actor }} disabled platform feature {{ audit.requestObject.spec.key }}"
    - name: create
      match: "!audit.user.username.startsWith('system:') && audit.verb == 'create'"
      summary: "{{ actor }} registered platform feature {{ audit.requestObject.spec.key }}"
```

Change reason on write: portal UI or annotation `services.miloapis.com/change-reason` on PATCH. Cloud portal activity log queries results via `AuditLogQuery`.

```mermaid
flowchart LR
    subgraph write [Operator action]
        Patch["PATCH spec.enabled"]
        Reason["Optional reason"]
    end
    subgraph record [Audit trail]
        K8sAudit["Apiserver audit log"]
        StatusHist["status.lastTransition + history"]
        Policy["ActivityPolicy auditRules"]
        Timeline["Activity timeline / AuditLogQuery"]
    end
    Patch --> K8sAudit
    Patch --> StatusHist
    K8sAudit --> Policy
    Policy --> Timeline
    Reason --> StatusHist
```

### Relationship to other mechanisms

```mermaid
flowchart TB
    subgraph catalog [services.miloapis.com Platform scope]
        SVC["Service"]
        PFF["PlatformFeatureFlag"]
        SVC -.->|optional owner| PFF
    end
    subgraph projectScope [Project scope]
        SE["ServiceEntitlement"]
    end
    subgraph orgScope [Organization scope]
        RR["ResourceRegistration type=Feature"]
        AB["AllowanceBucket per org"]
        RR --> AB
    end
    subgraph clients [Clients]
        Portal["Cloud portal OpenFeature"]
        Desktop["Datum desktop app"]
    end
    PFF --> Portal
    PFF --> Desktop
    AB -.->|"billing / tier UI flags"| Portal
    SE -.->|"different concern"| catalog
```

| Mechanism | Scope | Use for |
|-----------|-------|---------|
| `PlatformFeatureFlag` | Platform | Rollout kill switches, default-on product behavior shared across clients |
| Quota `Feature` + `AllowanceBucket` | Organization | Tier- and commerce-gated features |
| `ServiceEntitlement` | Project | Consumer opts into a managed service |

### Client integration (informative)

Implementation lives in consumer repos after this enhancement is approved.

**Cloud portal:** Extend Milo-backed OpenFeature provider (or composite). Platform keys read from root cluster API with no org `targetingKey`. Billing keys unchanged. Operator toggle reads `status.history` and sends change reason on PATCH.

```ts
await isPlatformFeatureEnabled(
  'networking.datumapis.com/auto-create-traffic-protection'
);
// false when off, not Active, or Milo API lookup fails (fail-closed)
```

**Datum desktop app:** Fetch flags at session start; cache ~5s; gate tunnel create traffic-protection auto-create; treat fetch failure as off.

**Optional admission:** Reject `TrafficProtectionPolicy` CREATE when linked platform flag is off.

## Production Readiness Review Questionnaire

### Feature Enablement and Rollback

#### How can this feature be enabled / disabled in a live cluster?

- [x] Other
  - Mechanism: PATCH `spec.enabled` on the cluster-scoped `PlatformFeatureFlag` object (Platform parent context / root API).
  - Control plane downtime: not required.
  - Node downtime / reprovisioning: not required. Clients pick up changes within cache TTL.

#### Does enabling the feature change any default behavior?

Yes for registered flags created with `spec.enabled: true`. The motivating flag turns on auto-create traffic protection on tunnel / edge create when enabled.

#### Can the feature be disabled once it has been enabled?

Yes. PATCH `spec.enabled: false`. Clients stop auto-provisioning on create within cache TTL. Existing resources are not deleted. Optional admission can block new CREATEs when off.

#### What happens if we reenable the feature if it was previously rolled back?

PATCH `spec.enabled: true`. New create flows resume auto-provision. No migration of resources created while the flag was off.

#### Are there any tests for feature enablement/disablement?

Planned: controller tests for history append; client integration tests for fail-closed eval; ActivityPolicy rule tests against sample audit payloads.

### Rollout, Upgrade and Rollback Planning

#### How can a rollout or rollback fail?

Clients on old builds may ignore platform flags and use env vars only. Mitigation: optional admission; deprecate env override after platform flag ships. Partial API outage causes fail-closed (auto-create off everywhere), which is intentional.

#### What specific metrics should inform a rollback?

Spike in tunnel create failures, traffic protection admission denials, or operator flag toggle frequency during an incident window.

#### Were upgrade and rollback tested?

Not yet. Manual test plan: toggle flag in staging, verify Datum desktop app and portal behavior within TTL.

#### Is the rollout accompanied by any deprecations?

Transitional: `DATUM_CONNECT_CREATE_TRAFFIC_PROTECTION_POLICIES` env override on Datum desktop app deprecated once platform flag is live.

### Monitoring Requirements

#### How can an operator determine if the feature is in use by workloads?

LIST `PlatformFeatureFlag` objects; check `status.enabled` and `status.lastTransition`.

#### How can someone using this feature know that it is working for their instance?

- [x] API .status
  - `status.enabled`, `status.lastTransition`, `status.history`
- [x] Other
  - Activity timeline entries from `ActivityPolicy` on PATCH

#### What are the reasonable SLOs for the enhancement?

Platform flag read path available for client eval at the same SLO as other root Platform API reads. Flag flip visible to clients within cache TTL (target ~5s).

#### What are the SLIs an operator can use to determine the health of the service?

- [x] Other
  - Client-side flag fetch error rate (Datum desktop app, portal)
  - Apiserver audit / activity policy match rate for flag PATCHes

#### Are there any missing metrics that would be useful?

Per-flag eval latency and cache hit rate on clients; not required for v0 doc approval.

### Dependencies

#### Does this feature depend on any specific services running in the cluster?

- **services.miloapis.com API (Platform context)**
  - Clients LIST/GET `PlatformFeatureFlag`
  - Outage: fail-closed (behavior off)
- **activity.miloapis.com (ActivityPolicy + AuditLogQuery)**
  - Human-readable audit timeline
  - Outage: audit summaries missing; apiserver audit and on-object history remain

### Scalability

#### Will enabling / using this feature result in any new API calls?

Yes. Clients poll or LIST platform flags with short TTL (~5s per client session). Volume scales with connected Datum desktop app sessions and portal requests, not with org count.

#### Will enabling / using this feature result in introducing new API types?

Yes. `PlatformFeatureFlag`, cluster-scoped. Expected object count: small (tens of flags, not thousands).

#### Will enabling / using this feature result in increasing size or count of the existing API objects?

`status.history` capped at 64 entries per flag. Bounded status growth.

#### Will enabling / using this feature result in non-negligible increase of resource usage?

Minimal etcd footprint (low object count). Controller work on PATCH only.

### Troubleshooting

#### How does this feature react if the API server is unavailable?

Clients fail-closed (treat flag as off). Auto-create WAF and similar default-on behaviors stop until API recovers.

#### What are other known failure modes?

- **Stale client cache after operator flip**
  - Detection: compare `status.lastTransition.changedAt` to client behavior delay
  - Mitigation: reduce TTL; restart client session
- **ActivityPolicy not deployed**
  - Detection: PATCH in audit log but no timeline summary
  - Mitigation: deploy `services.miloapis.com-platformfeatureflag` ActivityPolicy

#### What steps should be taken if SLOs are not being met to determine the problem?

Check root Platform API health, client flag fetch logs, and `PlatformFeatureFlag` object state. Query activity via `AuditLogQuery` for recent PATCHes.

## Implementation History

- 2026-06: Provisional enhancement drafted in `milo-os/service-catalog` ([PR #44](https://github.com/milo-os/service-catalog/pull/44))

## Drawbacks

- Another catalog resource type to implement, RBAC, and document.
- Fail-closed on API errors disables default-on behaviors platform-wide during outages.
- Two flag systems (platform vs org AllowanceBucket) require clear operator training.

## Alternatives

| Alternative | Why not |
|-------------|---------|
| Bulk update org `AllowanceBucket` Feature flags | No single switch; operational burden scales with org count |
| Env / build vars only | Not shared across Datum desktop app and portal; requires release to flip |
| `ServiceEntitlement` or org-wide service defaults | Wrong scope; models per-project or per-service opt-in, not global kill switches |
| Hard-coded feature gates in each client | No central audit, no operator UI, divergent defaults |

## References

- [Enhancement template](https://github.com/datum-cloud/enhancements/tree/main/enhancements/template)
- [`service-registry.md`](./service-registry.md)
- [`service-enablement.md`](./service-enablement.md)
- [`service-enablement-architecture.md`](./service-enablement-architecture.md)
- Cloud portal `app/modules/feature-flags/` (org AllowanceBucket; read-only, no ActivityPolicy)
- Cloud portal `app/resources/activity-logs/` (`AuditLogQuery`)
- `milo-os/billing/config/components/activity-policies/` (`ActivityPolicy` reference)
