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
- FR9: The platform must support **bring your own cloud**. A customer brings their own GCP
  project or AWS account; Datum integrates with network and infrastructure the customer has
  deployed there, and a Datum-authored controller operates against that account. The
  `LocationClass` for such a location lives in the customer's control plane and the customer
  manages it. This is a product requirement, not a hypothetical extension of FR7: FR7
  covers infrastructure a consumer owns and operates, whereas BYOC covers infrastructure a
  consumer owns and Datum operates on their behalf, inside their account.

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
- NFR6: For a BYOC location (FR9), Datum must not hold a credential that grants standing
  access into the customer's cloud account. Material Datum stores about such a location
  must be verification material, not access material. See
  [D16](#d16-store-verification-material-not-access-credentials).
- NFR7: The version of software Datum operates in a location and the version of the
  substrate that location runs on are separate axes. Each must be independently
  representable, with the desired value as input and the observed value as output, and a
  location running an out-of-range version must be distinguishable from one that is
  refused outright. See
  [D18](#d18-the-version-contract-has-two-axes-and-both-are-first-class).

---

## Design Review Outcomes (2026-08-24)

Ten decisions. D10 through D14 came out of the 2026-08-24 design review. D15 through D19
came out of the work that followed it: the BYOC product requirement (FR9), the types
landing in `milo-os/locations`, and a survey of how other platforms attach infrastructure
they do not own. Each records what it costs, and each names the part of this document it
replaces. Everything untouched stands as written, including the single-type recommendation
(Decision 1), the class selector model (Decision 3), the city-code API (Decision 4), the
quota dimension (Decision 5), and the three-gate model.

One decision was **reversed**: `spec.operatedBy`, proposed in an earlier draft of D12, is
deleted. See D12 for why the model already answers what it was for.

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
  resource, GatewayClass-style. It is optional, and it is deliberately **local-only**:
  there is no `namespace` and no `project` on it, so a class can only name parameters
  living in the same control plane as the class. See D17 for why that asymmetry with
  `locationClassRef` is the point rather than an oversight.

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
comes with the `Accepted` condition, now landed (Q8).

There is a second cost, in a different repository, and it is not theoretical. Both
`infra-provider-gcp` and `unikraft-provider` decide whether a `Location` is theirs in
`internal/locationutil/location.go`, and the *first* thing that function tests is
`location.Spec.Provider.GCP == nil` — the class name is only consulted afterwards.
Deleting `LocationSpec.Provider` therefore removes the primary gate in both operators,
not merely their GCP wiring. `unikraft-provider` is the sharper case: it carries a
byte-identical copy of that check, so it currently refuses any location that is not a GCP
location, which is plainly not what a Unikraft provider means to do. Both have to move to
selecting on `spec.locationClassRef` before `spec.provider` goes away.

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

#### What landed

The types are implemented in [milo-os/locations#4](https://github.com/milo-os/locations/pull/4)
(head `c28c391`; `LocationClass` itself arrived in `8b565e5`, and `c28c391` is the later
commit that added the project qualifier). The PR is **open**, and it is types only.

`LocationClass` is cluster-scoped in `locations.miloapis.com/v1alpha1`. `controllerName`
is required and immutable, enforced by a CEL rule
(`self == oldSelf`, message `controllerName is immutable`), with the documented escape
being to create a new class and move locations across. `parametersRef` is optional and is
`{group, kind, name}` only. `status.conditions` carries an `Accepted` condition with
reasons `Accepted`, `InvalidParameters`, and `Pending`.

`LocationSpec.Provider` and `GCPLocationProvider` are gone, and no GCP reference survives
anywhere in that repository. `LocationSpec` is now `locationClassRef`, `topology`, and
`coordinates`. `LocationBinding` is deleted as a kind, per D13.

`spec.locationClassRef` is `{name, project}`: `name` required, `project` optional with
empty meaning "a class that lives alongside this Location". That answers Q10 in the
affirmative and in the shape Q10 recommended.

Two things have **not** landed, and both matter for reading the rest of this document:

- **There is no `LocationClass` controller.** `internal/controller/` holds only the
  location publisher. Nothing reconciles a `LocationClass` and nothing ever writes the
  `Accepted` condition, so today every class sits at `Accepted` unset. The `Location`
  webhook is a logging-only stub that validates nothing. Every recommendation below that
  leans on `Accepted` as a control point — Q8, Q9, Q12, D18 — is describing a condition
  that no code sets yet.
- **A dangling class reference degrades rather than rejects.** `LocationClassRef`'s godoc
  states that naming a class that does not exist leaves the location not `Ready` rather
  than refusing the write. That is the right default, and it is the reason the missing
  controller is not currently visible as breakage: nothing is `Ready`, and nothing
  complains.

### D12: There is no operational-responsibility field

An earlier draft of this section proposed `spec.operatedBy` on `Location`, an enum of
`Platform` and `Consumer`. That field is deleted, and nothing replaces it.

The model already answers the question. A `LocationClass` lives in the control plane of
whoever owns the capacity it describes, and it carries `spec.controllerName` naming the
controller that acts on locations of that class. So "whose capacity is this" is answered by
which control plane holds the class, and "who operates it" is answered by
`spec.controllerName` on that class. An `operatedBy` enum restates the shape of the model
in a field, and a restatement is a second source of truth that can disagree with the first
without anything noticing.

The diagnosis that produced the field still stands, and D11 is what acts on it. Today
`datum-managed` and `self-managed` describe who runs the control plane while the `provider`
block on the same object describes who supplies the capacity. Those are orthogonal, and
fusing them into one class name produces a cross-product that has to be enumerated by
hand: a consumer-operated location whose capacity sits in GCP is expressible in the world
and not expressible in the enum, and the moment a second provider appears the enum needs
`self-managed-gcp`, `self-managed-bare-metal`, and so on. Making the class a resource is
what removes the cross-product. Adding a second enum beside it was never load-bearing.

`provider-dedicated` does not survive as a class value either, because it was not
describing capacity or operation. It described exclusivity: who is allowed to use this
location. That axis already has a field. `ownerProjectRef` set means the location is
dedicated to that project; absent means it is shared. So the existing values decompose,
and BYOC (D15) falls out of the same three columns without a fourth:

| Today's `locationClassName` | `spec.locationClassRef` | Which control plane holds the class | `spec.ownerProjectRef` |
|-----------------------------|-------------------------|-------------------------------------|------------------------|
| `datum-managed` | a Datum-operated class, e.g. `datum-gcp` | Datum's | unset |
| `provider-dedicated` | the provider's class | the provider's | set |
| `self-managed` | the consumer's class, e.g. `self-managed-bare-metal` | the consumer's | set |
| *(new)* BYOC | the customer's class, e.g. `acme-gcp` | the customer's | set |

The evidence that no field is needed is that nothing consumes the semantics of the values
we already have. In `api/v1alpha1/serviceavailability_types.go` the constants
`LocationClassDatumManaged`, `LocationClassProviderDedicated` and `LocationClassSelfManaged`
appear at their definition site and in exactly one test fixture, and nowhere else in the
repository. The only real consumer is `ServiceLocationConfig.SupportedClasses`, which
`locationBindingReconciler` turns into a `map[LocationClassName]struct{}` and tests for
membership. It never asks what a class means. Nothing anywhere branches on
platform-versus-consumer today, so a field encoding that distinction would ship with no
reader.

Meanwhile the class already works as a controller selector in production code:
`infra-provider-gcp` and `unikraft-provider` each take a location class name as operator
configuration and ignore every `Location` outside it. That is `spec.controllerName` avant
la lettre, and it is the mechanism D11 formalises.

This does not foreclose the field. If a single `LocationClass` ever has to serve two
different operators — the same capacity shape run by Datum for one consumer and by the
consumer themselves for another — then the class stops discriminating and an explicit
field earns its place. Adding an optional enum to `LocationSpec` at that point is additive
and breaks nothing, so the cost of waiting is zero and the cost of guessing now is a field
in a platform primitive that no controller reads.

This replaces the `locationClassName` Values table below and the `LocationClassName` enum
in `api/v1alpha1/serviceavailability_types.go`.
`ServiceConfiguration.spec.locations.supportedClasses` keeps its shape and its meaning
from Decision 3; its members become `LocationClass` names rather than enum members, which
is what makes the class name public API surface (Q13) and which reopens a set that is
closed today. See [Coordination Risk](#coordination-risk) for what that costs.

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

### D15: Bring your own cloud is a first-class requirement

FR9. A customer brings their own GCP project or AWS account; Datum integrates with network
and infrastructure the customer has already deployed there, and a Datum-authored
controller operates against that account. The `LocationClass` for such a location lives in
the **customer's** control plane and the customer manages it.

This falls out of D11 and D12 without a new mechanism, which is the argument for the class
model rather than an enum. The class is in the customer's plane, so "whose capacity" is
answered. `spec.controllerName` on it names a Datum controller, so "who operates it" is
answered, and it is answered with a value that differs from the `self-managed` case even
though both classes live in the customer's plane. An `operatedBy` enum would have had to
distinguish those two cases with a third value, because `Consumer` is wrong (Datum
operates it) and `Platform` is wrong (the customer owns the account). The class model
distinguishes them for free.

BYOC is the case that makes the credential question (D16) and the reference-scoping
question (D17) load-bearing rather than hygienic, because it is the first case where a
Datum controller acts on configuration written by someone outside Datum.

### D16: Store verification material, not access credentials

For BYOC, Datum should run a controller **in the customer's account, under the customer's
own identity**, and hold no cloud credential for that account itself.

The surveyed prior art splits cleanly on this. GKE's `AttachedCluster` stores
`oidcConfig.jwks`, "OIDC verification keys in JWKS format (RFC 7517)" — public key
material, and optional at that, needed only when the cluster has no publicly reachable
discovery endpoint. Azure Arc-enabled Kubernetes stores
`properties.agentPublicKeyCertificate`, "Base64 encoded public certificate used by the
agent to do the initial handshake to the backend services in Azure." In both, the material
held by the cloud is public, it verifies an inbound connection, and neither cloud can
reach inward.

The counter-example is GKE Multi-Cloud's `AwsCluster`, which holds
`awsServicesAuthentication.roleArn` naming "the role that the Anthos Multi-Cloud API will
assume when managing AWS resources on your account." Google reaches in. That is the
largest blast radius of the set: a compromise of the managing service is a compromise of
every customer account that trusts it. It is also the one being withdrawn — GKE on AWS
entered maintenance mode on 2025-03-17 and is
[supported until 2027-03-17](https://docs.cloud.google.com/kubernetes-engine/multi-cloud/docs/aws/deprecations/deprecation-announcement),
at which point the APIs are disabled. Note the direction of travel precisely:
`AttachedCluster` is not deprecated, it is the recommended migration target. The industry
moved from the assume-a-role model to the verify-an-agent model.

An in-account controller inherits that property. Datum holds nothing that can be stolen to
reach a customer account, the customer can revoke by removing the controller's identity in
their own IAM, and the audit trail lives in the customer's account where their compliance
team can already see it.

Datum's existing GCP path is already credential-free in the relevant sense and is worth
preserving as the precedent: `infra-provider-gcp` never creates a Crossplane
`ProviderConfig`, it only references one by name, and the manifests in `datum-cloud/infra`
use `credentials.source: InjectedIdentity` (Workload Identity) for the deployment control
plane and `credentials.source: ImpersonateServiceAccount` per project. No service account
key exists anywhere in that path. See Q14 for what is genuinely unresolved.

### D17: Scope the references, not just the object

`LocationClass.spec.parametersRef` is **local-only** — resolvable only within the control
plane that holds the class — and `spec.locationClassRef` legitimately crosses planes. That
asymmetry is deliberate and it is the whole of this decision.

The failure mode is making an object namespaced or project-scoped and then leaving its
outbound references unscoped, so the object is contained but its reach is not. Crossplane
v2 is the worked example: `ProviderConfig` became namespaced, but the credential selector
still embeds `SecretReference`, whose godoc says in as many words that it is "a reference
to a secret in an arbitrary namespace" and whose `Namespace` field carries neither
`omitempty` nor `+optional`. A namespaced `ProviderConfig` therefore still reads Secrets
in other namespaces. The same file defines `LocalSecretKeySelector`, "a reference to a
secret key in the same namespace with the referencing object", and it is not used for
provider credentials — its only consumers are the package `ImageConfig` types. (These now
live in `crossplane/crossplane` under `apis/core/v2`, not in `crossplane-runtime`, which
no longer carries the common API types.) The scoped type existed and was not applied where
it mattered most.

The landed shape already gets this right, and it should be defended rather than
"fixed" later: `ParametersReference` is `{group, kind, name}` with no `namespace` and no
`project`, so a class can only name parameters in its own plane, while
`LocationClassReference` is `{name, project}` precisely because it must cross. If a future
change adds a `project` to `parametersRef` for convenience, a customer-authored class in
the customer's plane gains the ability to point a Datum controller at a resource inside
Datum's plane, which is the D18 concern in reverse and much worse.

### D18: The version contract has two axes, and both are first-class

There are two independent version axes and they must not be collapsed: **our software
version** (the controller or agent Datum ships into a location) and **the customer's
substrate version** (the cluster, the cloud API, the hardware). The prior art consistently
separates them as an input the operator sets and an output the system observes:

| System | Input (desired) | Observed |
|--------|-----------------|----------|
| GKE `AttachedCluster` | `platformVersion` (required) | `kubernetesVersion` (output-only) |
| Azure Arc | `arcAgentProfile.desiredAgentVersion` | `agentVersion` (read-only) |
| GDC Edge | `targetVersion` (optional) | `nodeVersion` (output-only) |

Read that table with three caveats, each of which matters for copying the pattern. The
GKE split is specific to `AttachedCluster`; `AwsCluster` and `AzureCluster` have only a
required `controlPlane.version` with no observed counterpart. Arc's N-2 support window is
stated twice, once for agents and once for upstream Kubernetes — it is a support-request
policy, not something the API enforces, and it is not one window per field. And GDC Edge's
`nodeVersion` is "the lowest release version among all worker nodes" and may legally be
empty when there are no worker nodes, so it is a min-aggregate with a null case, not a
scalar.

**Recommendation: adopt Gateway API's two-condition split on `LocationClass`** — `Accepted`
(will this controller serve this class at all) beside `SupportedVersion` (is the detected
version in range). The value is that it distinguishes *degraded but working* from
*refused*, which one boolean cannot, and it lets the platform tighten from warn to refuse
later by moving which condition goes false without changing the schema or breaking readers.

Two things to get right, and the first is where an earlier draft of this section was
wrong. Gateway API does **not** mandate failing closed. What
`apis/v1/gatewayclass_types.go` requires is only that the *condition* be set false: "If
implementations detect any Gateway API CRDs that either do not have this annotation set,
or have it set to a version that is not recognized or supported by the implementation,
this condition MUST be set to false." Behaviour is then an explicit choice between two
`MAY`s — best-effort support, signalled as `Accepted=true` with `SupportedVersion=false`,
or refusal, signalled as `Accepted=false` with reason `UnsupportedVersion`. Fail-closed is
a decision this design has to make on its own merits; it cannot be borrowed as a `MUST`.
For a location, where the failure mode is a controller acting on a substrate it does not
understand inside a customer's account, refusing is the right default — but it should be
recorded as our choice.

Second, the message: Gateway API says one "SHOULD be included in this condition that
includes the detected CRD version(s) present in the cluster and the CRD version(s) that
are supported". Naming both sides is what makes the condition actionable, and it is cheap.

Finally, the maturity of the thing being copied. `SupportedVersion` carries a
`<gateway:experimental>` marker — inert in practice, since conditions are not schema
fields and the string appears in neither channel's CRDs, while the implementers guide
states it as an unqualified MUST. It has **zero conformance tests**. The shape is worth
borrowing; the guarantees are not there to lean on.

### D19: Deletion protection is a finalizer on the protected object

**Do not build deletion protection out of usage-marker objects that are owned by the thing
they protect.** Put a finalizer on the protected object itself.

Crossplane's `ProviderConfigUsage` is the pattern to avoid, and its failure is structural
rather than a fixable bug. The usage marker sets `blockOwnerDeletion: true` on its owner
reference and carries no finalizer of its own, so under foreground cascading deletion the
marker is deleted before its owner finishes terminating, and the blockage it was supposed
to provide is gone at exactly the moment it is needed. Crossplane maintainer `turkenh`
states the mechanism plainly in
[crossplane#4661](https://github.com/crossplane/crossplane/issues/4661): "resources are
being deleted starting from the bottom of the tree and hence `ProviderConfigUsage` being
owned by the `Object` is deleted before the actual `Object` is deleted. This removes the
blockage on `ProviderConfig`." That issue was opened 2023-09-22 and **is still open**, with
a maintainer noting in October 2025 that it is being looked at for the v2.2 timeframe.
[crossplane#5849](https://github.com/crossplane/crossplane/issues/5849) is an independent
reproduction, closed as a duplicate of it.

There is a second, distinct failure with the same root:
[crossplane-runtime#1010](https://github.com/crossplane/crossplane-runtime/issues/1010)
(open, 2026-05-29) is a namespace-deletion deadlock in which the tracker tries to *create*
a usage marker in a `Terminating` namespace, the API server refuses, and the provider never
reaches its delete path, so the finalizer never clears. Both bugs come from making
deletion safety depend on the lifecycle of a second object.

One correction to an earlier reading of this evidence, because it changes the conclusion's
support: Crossplane has **not** abandoned the pattern.
[crossplane#7362](https://github.com/crossplane/crossplane/pull/7362) (merged 2026-05-08)
is sometimes cited as the fix; it is not. It protects *Provider packages*, not
`ProviderConfig`s, it explicitly "reuses existing Usage infrastructure rather than
introducing a new webhook", and it is alpha behind
`--enable-provider-deletion-protection`. What it does introduce, and what is worth taking,
is deriving protection from **watching real instances** rather than trusting a marker to
be present. `ProviderConfigUsage` remains unfixed. So the lesson is not "even Crossplane
moved on" — it is that the pattern has been broken for three years and its owners have not
yet found a cheap way out.

**Recommendation: `gateway-exists-finalizer`.** Gateway API puts
`gateway-exists-finalizer.gateway.networking.k8s.io` on the `GatewayClass` — the protected
object — whenever one or more `Gateway`s use it. No second object, nothing to race, and
the protection is visible on the object a human would look at. Note it is a `SHOULD` and
not a `MUST` upstream. For us: a finalizer on `LocationClass` while any `Location`
references it, which composes with Q9's rule that deleting the parameters resource marks
the class not `Accepted` and blocks new provisioning without withdrawing running capacity.

### Prior art: AWS availability zones

NFR3 and Decision 1 already cite AWS's `ZoneType` approvingly against Azure's
`customLocations`. It is worth stating how much further the parallel goes, because
`DescribeAvailabilityZones` is this entire design in miniature on one shared API surface:

- **`ZoneType`** (`availability-zone` | `local-zone` | `wavelength-zone`) is a
  discriminator that changes what a zone *means* without forking the type — exactly the
  role `locationClassRef` plays here.
- **`parentZoneName` / `parentZoneId`** are an explicit pointer to where the controlling
  plane sits, for zone types that are extensions of a parent region. That is
  `locationClassRef.project` (D11) and `ownerProjectRef` (Decision 6), and it is the same
  insight Q10 arrived at independently: a cross-plane reference is a name plus a plane.
- **`zoneState`** is not a boolean. The enum is
  `available | information | impaired | unavailable | constrained`, and AWS's prose
  narrows it to "`available`, `unavailable`, and `constrained`". The middle value is the
  interesting one: `constrained` is degraded-but-usable, which is the same distinction D18
  wants from `Accepted` beside `SupportedVersion`. A location primitive whose readiness is
  one boolean cannot express "this works but stop scheduling new capacity here".
- **`optInStatus`** (`opt-in-not-required` | `opted-in` | `not-opted-in`) is three-valued
  per-account activation of a shared location, and it is D14's `SelfService` versus
  `GatedByProvider` distinction expressed as state on the location rather than as a
  separate request kind. Worth weighing against `LocationEntitlement`: AWS chose a field,
  we chose a resource, and the resource is right only because our activation carries an
  approval handshake and a message that AWS's does not.

### What this replaces in the sections below

| Section | Status |
|---------|--------|
| Recommendation: Single `Location` Type | Group guidance replaced by D10; the single-type conclusion stands and D13 strengthens it |
| `locationClassName` Values | Replaced by D11 and D12 |
| Project-Level `LocationBinding` (new resource) | Replaced by D13 |
| Platform-Level `Location`, and all `Location` examples | `spec.provider` replaced by D11; `spec.locationClassName` replaced by `spec.locationClassRef` |
| Two-Tier Discovery Model, Three-Gate Model, lifecycle, Consumer-Facing API | Mechanism unchanged; every `LocationBinding` reads as "projected `Location`" |
| Security Considerations | The "provider-internal fields are never copied" argument is moot under D11; the fields are not on `Location` |
| Decision 9, Migration Path phase 4 | Replaced by D10 |
| Q2, Q6 | Answered by D14 and D10 respectively |
| Q7, Q8, Q10 | Answered by the types that landed in `milo-os/locations#4` |
| Decision 6 and the `ownerProjectRef` webhook rule | Does not reconcile with D11/D12; reopened as Q16 |

---

## Coordination Risk

This document now describes a target that the live code does not agree with. The
implementation of the new types landed in `milo-os/locations`, and this repository's own
`internal/controller/locationbinding_controller.go` breaks against it in three places.
None of them break at build time, because that reconciler reads and writes `Location` and
`LocationBinding` as `unstructured` objects against hard-coded GVK constants. D10 counted
that as the thing that makes the group move cheap. It is also the thing that makes the
cutover silent.

The three break points, all in `internal/controller/locationbinding_controller.go`:

1. **It creates a kind that no longer exists.** `locationBindingGVK` at lines 34-37 pins
   `networking.datumapis.com/v1alpha`, `Kind: LocationBinding`. D13 deletes that kind. On
   a control plane where the CRD is gone, every write fails `NoMatchError`. The cleanup
   path already tolerates that error; the write path does not.
2. **It reads a field that has changed shape.** Line 306 does
   `unstructured.NestedString(loc.Object, "spec", "locationClassName")`. On a new
   `Location` that field is `spec.locationClassRef`, an object rather than a string, so
   `NestedString` returns the empty string and the `!= ""` guard leaves `fields.class`
   empty. This is the dangerous one, and it is worse than a skip: the empty class flows
   into `evaluateGates`, misses the `supported` set, and every location in every project is
   projected with `Available=False`, reason `LocationClassNotSupported`, and the message
   `This service isn't offered for "" locations.` Nothing errors, nothing requeues, no
   alert fires. Every consumer loses every location at once, and the only evidence is a
   message quoting an empty string.
3. **It writes the old field into the object it creates.** Line 354 puts
   `"locationClassName": string(fields.class)` into the binding spec, which under (2) is
   the empty string, and which under D13 is a field on a kind that is going away.

A fourth break lives outside this repository and is easy to miss because it is in a
different language of the same mistake. `infra-provider-gcp` and `unikraft-provider` both
decide whether a `Location` belongs to them in `internal/locationutil/location.go`, and the
first test is `location.Spec.Provider.GCP == nil`, with the class name consulted only
afterwards. D11 deletes `spec.provider`, which removes the primary gate in both operators.
Both must move to selecting on `spec.locationClassRef` before that field goes away. While
looking: `unikraft-provider` carries a byte-identical copy of the GCP check and therefore
refuses any location that is not a GCP location, which is a pre-existing bug independent of
this migration.

The dual-write in D13 step 2 is what has to absorb the first three, and the ordering
constraint is that the reconciler must learn to read `spec.locationClassRef` **before**
any `Location` is written in the new shape — not at the same time, and not after. A
`Location` carrying only the new field, read by a reconciler that only knows the old one,
is the (2) failure mode in production.

### Latent defect: an unbounded `Location` name produces an invalid selector

Not a cutover problem — this is live in shipped code today, in both
`network-services-operator` and `milo-os/locations`, which carry byte-identical copies of
the location publisher.

`applyPropagationPolicy` writes the `Location`'s own `metadata.name` verbatim into a
Kubernetes **label value**, as the cluster selector on the generated
`ClusterPropagationPolicy`:

```go
"matchLabels": map[string]any{
    networkingv1alpha.ServingLocationTopologyLabel: name,   // "topology.datum.net/location"
}
```

Label values are capped at 63 characters. Object names are DNS subdomains and allow 253,
and **nothing bounds a `Location` name**: `metadata` is a bare `type: object` in both
CRDs, the only `maxLength: 253` markers on the new type are on
`spec.locationClassRef.{name,project}`, NSO has no `Location` admission webhook at all,
and the one in `milo-os/locations` is a logging-only stub. So a `Location` named with more
than 63 characters is admitted, and the policy apply then fails validation forever. The
location publishes and its objects never propagate to any cell. Latent, not live: no
current location is long enough.

The same unbounded name reaches two more places. `locationPropagationPolicyName` is just
`"location-" + name`, so a 253-character location yields a 262-character policy name,
over the 253-character object-name limit as well. And `networking.datumapis.com/location`
carries the name as a label value on `NetworkContext` and `Subnet` selectors by the same
route.

Crossplane shipped this exact bug:
[crossplane#3224](https://github.com/crossplane/crossplane/issues/3224), "ProviderConfigUsage
creation blocked because providerconfig name > 63 characters", whose body states the
mismatch precisely — names allow 253, label values allow 63 — and reports the concrete
`metadata.labels: Invalid value: … must be no more than 63 characters`. (Cite it for the
mechanism, not as a general "names in labels" bug: the subject there is specifically a
`ProviderConfig` name copied into a label.)

**Recommendation: a CEL rule on `Location` bounding `metadata.name` to 63 characters**,
enforced at the CRD so it fails at admission with a comprehensible message rather than in
a reconcile loop. 63 rather than 253 because the label value is the binding constraint,
and bounding the name is far cheaper than hashing it and losing the readable selector.
This should land before BYOC (D15), where customers choose their own location names and
the 63-character ceiling stops being a theoretical limit.

### The class enum is a live closed set

Separately, `LocationClassName` in `api/v1alpha1/serviceavailability_types.go` is a real
`+kubebuilder:validation:Enum=datum-managed;provider-dedicated;self-managed`, and
`ServiceLocationConfig.SupportedClasses` is typed `[]LocationClassName`. Making the class
a name reference (D11, D12) opens that set: any string a provider chooses for a
`LocationClass` becomes a legal member, and the API server stops rejecting typos.

The type's own doc comment says the closed set exists "so that controllers and IAM
constraints can branch on a closed set". D12 establishes that nothing branches on it
today, which is why dissolving it is safe now. But the comment states an intent, and the
intent is the tension: the moment an IAM policy or a controller does want to branch on
class, it will be branching on an open set of provider-authored strings, which is a
different and weaker thing to write a policy against. Q13 already treats the class name as
public API surface for the human-readable reason. This is the machine-readable half of the
same problem, and it is not settled here.

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
> a resource covering capacity and provider; which control plane holds the class, and the
> `spec.controllerName` on it, cover who operates the location; and `spec.ownerProjectRef`
> covers exclusivity. See the decomposition table in D12 for how each value maps. New
> classes no longer require a platform-level enum change at all; a provider creates a
> `LocationClass`.

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
> `locations.miloapis.com/v1alpha1`, `spec.locationClassName` becomes
> `spec.locationClassRef` naming a `LocationClass`, and the `spec.provider` block in the
> example below is deleted from the type. Its GCP fields live in an `infra-provider-gcp`
> parameters CRD that the class references. Nothing replaces `spec.provider` on
> `Location`. Read the example for the topology and condition shape, which are unchanged,
> not for the group or the provider block.

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

> **Amended by D12 and D14.** Read `spec.locationClassName: self-managed` as a
> `locationClassRef` to a consumer-supplied `LocationClass` living in the consumer's own
> control plane, and `provider-dedicated` as a reference to a Datum-operated class with
> `ownerProjectRef` set. The four control points themselves are unchanged, and D14 gives
> the third one a concrete mechanism: a `GatedByProvider` `LocationEntitlement` approved
> by the provider.

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
> control plane. The `CLASS` column becomes `spec.locationClassRef.name`. There is no
> operator column: the class name is what the consumer reads, which is why Q13 treats it
> as public API surface. The list itself, one filtered view of what the project can use,
> is unchanged.

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
> through `spec.locationClassRef`, which is why Q13 treats it as public API surface.

---

## Migration Path

| Phase | Current state | Target state | Action |
|-------|--------------|--------------|--------|
| 0 (now) | `Location` type in NSO; webhook lists all platform locations globally | Shared type module with `ownerProjectRef` field | Extract type to shared module; add `ownerProjectRef` field and `locationClassName` enum; no behavior change |
| 1 | Webhook uses cluster-wide `Location` list; no per-project visibility | `LocationBinding` per project; webhook uses project-scoped list; three-gate model enforced | Implement `ServiceAvailability` CRD + `LocationBindingReconciler` in `milo-os/service-catalog`; compute service operator creates `ServiceAvailability` on each PoP deployment; update webhook to use `LocationBinding` list; add `locations` section to compute `ServiceConfiguration` |
| 2 | No location dimension on `ResourceClaim`; no dedicated location support | Location dimension populated on all new claims; `provider-dedicated` bindings created on `Location` creation | Update `InstanceReconciler` claim construction; `LocationBindingReconciler` handles non-`datum-managed` classes via `ownerProjectRef` |
| 3 (future) | `self-managed` location registration is manual | Consumer-facing registration workflow | Consumer-facing API and automation for registering self-managed sites; may include a `LocationRegistration` request resource |
| 4 | `networking.datumapis.com/v1alpha` `Location` and `LocationBinding` | `locations.miloapis.com/v1alpha1` `Location` and `LocationClass` in `milo-os/locations`; no `LocationBinding` | D10 and D13. Stand up `milo-os/locations`; install both CRDs; dual-write the projected `Location` beside the existing `LocationBinding`; move readers; stop writing `LocationBinding` and let the existing owner-scoped prune sweep it; uninstall the CRD. Steps are detailed under D13 |
| 5 | `LocationSpec.Provider` with a `gcp` member; `locationClassName` as an enum | `LocationClass` resource with `controllerName` and `parametersRef`; `spec.locationClassRef` on `Location` | D11 and D12. `infra-provider-gcp` ships its own parameters CRD and a `LocationClass` per offering; delete `LocationProvider` and `GCPLocationProvider` from the core type; replace the `LocationClassName` enum in `serviceavailability_types.go` with a name reference. See [Coordination Risk](#coordination-risk) |
| 6 | Projection is always derived from the entitlement | `SelfService` derived projection plus a `GatedByProvider` `LocationEntitlement` request flow | D14. Reuse the `ServiceEntitlement` and `ServiceConsumer` mirroring and `spec.approval` handshake rather than reimplementing it |

Phase 4 supersedes the earlier "future, separate RFC" framing of the group move: it is
now a planned phase with a concrete dual-write path, and it does not block Phases 1
through 3, which can be built against the current group and carried across by the
dual-write. Phase 5 depends on Phase 4, because the class reference is introduced on the
new type rather than retrofitted onto the old one. Phase 6 depends on
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

The ten decisions below are stated in full, with their tradeoffs, under
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

- **Decision 12: There is no operational-responsibility field.** `spec.operatedBy` was
  proposed and is deleted. A `LocationClass` lives in the control plane of whoever owns the
  capacity it describes, so which plane holds the class answers "whose capacity" and
  `spec.controllerName` on it answers "who operates it". A field restates the shape of the
  model and becomes a second source of truth that can disagree with the first. The
  diagnosis that produced it still holds and D11 acts on it: who runs a location and who
  supplies its capacity are orthogonal, and fusing them into a class name produces a
  cross-product enumerated by hand. Making the class a resource is what removes the
  cross-product. Evidence that no field is needed: the three `LocationClassName` constants
  appear only at their definition site and in one test fixture, and the only real consumer,
  `SupportedClasses`, is a set-membership check over opaque strings — nothing branches on
  what a class *means*. `provider-dedicated` still does not survive, because it described
  exclusivity, which `ownerProjectRef` already carries. The field can be added later,
  additively, if one class ever has to serve two different operators.

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

- **Decision 15: Bring your own cloud is a first-class requirement (FR9).** Customers bring
  their own GCP projects and AWS accounts; Datum integrates with infrastructure deployed
  there and a Datum-authored controller operates with access to the account. The
  `LocationClass` lives in the customer's control plane and the customer manages it. This
  needs no new mechanism on top of D11 and D12, which is the strongest argument for the
  class model: BYOC and `self-managed` both put the class in the customer's plane and are
  told apart by `spec.controllerName`, a distinction an `operatedBy` enum could not have
  drawn without a third value.

- **Decision 16: Store verification material, not access credentials.** GKE
  `AttachedCluster` stores `oidcConfig.jwks` and Azure Arc stores
  `agentPublicKeyCertificate` — both public, neither lets the cloud reach inward. GKE's
  `AwsCluster` instead holds a `roleArn` Google assumes; it has the largest blast radius of
  the surveyed set and is the one being withdrawn (GKE on AWS is supported until
  2027-03-17). Recommendation: an in-account controller under the customer's own identity,
  with no Datum-held cloud credential. Datum's existing GCP path is already keyless
  (Workload Identity plus per-project impersonation), which is the precedent to preserve.
  See Q14 for what remains unresolved.

- **Decision 17: Scope the references, not just the object.**
  `LocationClass.spec.parametersRef` is local-only — resolvable only within the class's own
  control plane — explicitly contrasted with `locationClassRef`, which legitimately
  crosses. Crossplane v2 is the cautionary case: it made `ProviderConfig` namespaced but
  left `secretRef.namespace` required, so a namespaced config still reaches Secrets
  elsewhere, and the `LocalSecretKeySelector` type it built for exactly this was not used
  there. The landed `ParametersReference` is `{group, kind, name}` with no namespace and no
  project, which is already correct and should be defended rather than relaxed.

- **Decision 18: The version contract has two axes, both first-class (NFR7).** Our software
  version and the customer's substrate version are separate, and the prior art separates
  desired input from observed output (GKE `platformVersion`/`kubernetesVersion`, Arc
  `desiredAgentVersion`/`agentVersion`, GDC Edge `targetVersion`/`nodeVersion`).
  Recommendation: Gateway API's two-condition split, `Accepted` beside `SupportedVersion`,
  so degraded-but-working is distinguishable from refused and the platform can tighten from
  warn to refuse without a schema change. Fail-closed on missing version metadata is **our**
  choice, not an inherited `MUST` — Gateway API permits best-effort explicitly. The message
  should name both the detected and the supported versions. The upstream condition is
  marked experimental and has no conformance test.

- **Decision 19: Deletion protection is a finalizer on the protected object.** Not
  usage-marker refcounting: Crossplane's `ProviderConfigUsage` sets
  `blockOwnerDeletion: true` with no finalizer of its own, so foreground deletion removes
  the marker before its owner finishes terminating (crossplane#4661, open since 2023), with
  a namespace-deletion deadlock variant in crossplane-runtime#1010. Note Crossplane has not
  actually escaped the pattern — the often-cited #7362 protects Provider *packages* and
  reuses usage markers. Recommendation: a finalizer on the protected object in the shape of
  Gateway API's `gateway-exists-finalizer`, placed on `LocationClass` while any `Location`
  references it.

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

- **Q7: `parametersRef` versus an inline `RawExtension` on `LocationClass`.**
  **Answered by the implementation.** `milo-os/locations#4` landed
  `ParametersReference{Group, Kind, Name}`, optional, with no inline alternative. The
  reasoning below stood and is kept because it is also the reasoning for D17. Decision 11
  settles that provider configuration leaves `Location`; it did not settle how the class
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
  **Answered in the type, not yet in behaviour.** `milo-os/locations#4` landed the
  `Accepted` condition with reasons `Accepted`, `InvalidParameters`, and `Pending` —
  exactly the recommendation. But no controller sets it, so today the condition is
  unset on every class. The type is settled; the behaviour is not built. Original
  recommendation, retained for its reasoning: **a GatewayClass-style `Accepted`
  condition**, with reason
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

- **Q10: The class reference must carry a project qualifier.** **Answered by the
  implementation, in the recommended shape.** `LocationClassReference` is `{name,
  project}`, with `project` optional and empty meaning "a class that lives alongside this
  `Location`". Its godoc states the intent directly: "Set it when you are consuming
  capacity somebody else operates: the class stays in their control plane, where they own
  it, and your Location points across at it."

  Two notes for implementation. `milo-os/ipam`'s `ClassSourceRef` is the closest in-house
  precedent and goes one step further — its doc names the RBAC verb that authorises the
  cross-project reference (`ipam.miloapis.com/ipclasses.use`, "checked when the
  referencing class is created"). `LocationClassReference` has no equivalent permission
  story, which is the substance of Q15. IPAM also keeps two types, a mandatory-project
  `ClassSourceRef` for cross-project references and a bare `LocalRef{Name}` for local
  ones, where locations collapses both into one optional field; the collapsed form is
  friendlier and makes "did the author mean local?" unanswerable from the object alone.

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

- **Q13: The class name is public API surface.** A consumer reads
  `spec.locationClassRef` on a projected `Location` but cannot read the `LocationClass`
  itself: it is in another control plane behind another IAM boundary. The name is
  therefore the entire human-readable description of what that location is, and it
  appears in
  `ServiceConfiguration.spec.locations.supportedClasses`, in `datumctl` output, and in
  support conversations. Recommendation: treat class names as immutable once used and
  document a naming convention before the first provider ships one, rather than letting
  the first `LocationClass` set the precedent by accident.

The questions below were opened by the BYOC requirement (FR9) and the work behind D15
through D19. Each carries a recommendation.

- **Q14: What credential source does a BYOC provider configuration use?** Datum's current
  GCP path is already keyless — `infra-provider-gcp` references a Crossplane
  `ProviderConfig` by name and never creates one, and the manifests in `datum-cloud/infra`
  use `credentials.source: InjectedIdentity` for the deployment control plane and
  `credentials.source: ImpersonateServiceAccount` per project. That is the concrete
  machinery behind `GCPLocationProvider.ProjectID`'s doc comment, which says "a service
  account will be required for each unique GCP project ID". Read that comment as
  *impersonated service account*, not *service account key*.

  So the open question is not "are we using keys" but **how a BYOC customer grants that
  impersonation across an organisation boundary**. It matters that the answer cannot be
  "the customer mints a key and hands it to us": GCP enforces
  `iam.disableServiceAccountKeyCreation` and `iam.disableServiceAccountKeyUpload`
  [by default](https://docs.cloud.google.com/resource-manager/docs/organization-policy/restricting-service-accounts)
  for any organisation created on or after 2024-05-03 — and Google notes some
  organisations created between February and April 2024 may also have them — so a
  key-based onboarding flow fails outright for every customer organisation younger than
  that, with no code change on our side able to fix it. (Precision if this is quoted: the
  secure-by-default baseline is seven constraints, not two, and the creation constraint is
  named `constraints/iam.managed.disableServiceAccountKeyCreation` in its managed form.)
  Recommendation: settle this as part of D16 — an in-account controller running under an
  identity the customer creates in their own IAM sidesteps the question entirely, because
  Datum never needs a credential that crosses the boundary.

- **Q15: RBAC cannot constrain which class a `Location` references.** Kubernetes RBAC
  grants verbs on kinds; it cannot say "you may create a `Location`, but only one pointing
  at *these* classes". If a consumer can create a `Location` in their own control plane at
  all, they can set `locationClassRef` to any class name and project they like. Crossplane
  documents the same limit for `ProviderConfig` in its
  [v1.20 multi-tenancy guide](https://docs.crossplane.io/v1.20/guides/multi-tenant/),
  though more weakly than it is sometimes quoted: what the page actually says is that
  "anyone who has been given [RBAC] to manage `RDSInstance` objects can use any
  credentials to do so," and that "Crossplane assumes that only folks acting as
  infrastructure administrators or platform builders will interact directly with
  cluster-scoped resources." That is an assumption of trust, not a mechanism. The remedies
  the guide is built around are the three that exist for anyone: do not expose the
  referencing resource directly (compose it behind a claim), put an external admission
  policy engine in front of it, or give each tenant their own cluster.

  Recommendation: the reference is authorised on the *class*, not the `Location` — follow
  `milo-os/ipam` and require a `locationclasses.use` permission on the referenced class,
  checked at admission when the referencing `Location` is created. That turns "which
  classes may I name" into an IAM question on an object the class owner controls, which is
  the only place it can be answered. Cross-plane references make this mandatory rather
  than optional: a bare name plus project is otherwise an unauthenticated pointer into
  someone else's control plane.

- **Q16: The `ownerProjectRef` webhook rule does not reconcile, and is still open.**
  Decision 6 and the api-dev notes require `ownerProjectRef` to be absent for
  `datum-managed` and present for `provider-dedicated` and `self-managed`. D11 and D12
  dissolve all three values, so the field the webhook was to key on is gone. The review
  restated the invariant in terms of dedication rather than class, and D12 removed
  `operatedBy`, which the restatement then depended on. Both attempts have now failed, and
  papering over it a third time would be worse than leaving it open.

  The difficulty is real rather than editorial: exclusivity is a property of a
  *`Location`*, but the only thing left to infer it from is the *class*, and one class can
  legitimately back both shared and dedicated locations. BYOC (D15) makes this concrete —
  a customer's class in a customer's plane is always dedicated, while a Datum class may
  back either. Options: (a) make `ownerProjectRef` unconditionally optional and enforce
  nothing, letting the projection logic treat "set" as "dedicated"; (b) add
  `LocationClass.spec.dedicated` as a boolean the class asserts, and have the webhook
  require `ownerProjectRef` exactly when the referenced class asserts it, which costs a
  cross-plane read at admission; (c) require `ownerProjectRef` whenever
  `locationClassRef.project` is set or the class lives outside Datum's plane. Recommendation:
  **(a) for now**, because the projection logic already keys on the field's presence and
  nothing today depends on the invariant being enforced, and revisit with (b) if a class
  ever needs to assert exclusivity for its locations. Note this is the same shape as D12:
  the honest answer may again be that the class already carries the fact and no `Location`
  field should restate it.

- **Q17: A customer-authored `LocationClass` is configuration a Datum controller
  dereferences.** Under D15 the class lives in the customer's plane and the customer
  writes it, including `parametersRef` and `controllerName`. A Datum controller that
  resolves that reference and acts on what it finds is being pointed at a target chosen by
  someone outside Datum. The shape has a name in the Crossplane community — a
  [May 2026 discussion](https://github.com/crossplane/crossplane/discussions/7392) on
  namespaced `ProviderConfig` in multi-tenant environments describes tenant configuration
  that "can cause the controller to authenticate against tenant-controlled endpoints using
  its own ambient identity," and a replying commenter labels it SSRF-via-configuration.
  Weigh that citation accordingly: both the opening post and the reply are from
  non-members, and the discussion is still marked unanswered with no maintainer
  engagement. The mechanism is worth taking seriously; the label carries no official
  weight and Crossplane has not characterised it either way.

  Recommendation: D16 removes the dangerous half by construction. A controller running
  in-account under the customer's own identity has no ambient Datum identity to abuse, so
  the worst outcome of a maliciously authored class is that the customer directs their own
  credentials at their own targets. D17 removes the other half by keeping `parametersRef`
  local, so a customer's class cannot name a resource inside Datum's plane. Both should be
  stated as security properties of the design rather than left as incidental.

- **Q18: Validate at registration, not at first use.** Three failures worth not repeating,
  each from a system that attaches infrastructure it does not own:
  - **AWS EKS Connector** exposes no agent version on the API — `ConnectorConfigResponse`
    carries only `activationCode`, `activationExpiry`, `activationId`, `provider`, and
    `roleArn` — and AWS documents no upgrade procedure, so agent staleness is not
    observable through the API at all. (It is not deprecated; the gap is the absence of a
    version field, not abandonment.)
  - **Azure Arc** accepts `arcAgentProfile.agentAutoUpgrade` as an input and echoes the
    desired value back on GET, but exposes no *effective* in-cluster counterpart, so you
    can read what was asked for and not what is true. Arc gets the harder half right:
    `agentVersion` beside `desiredAgentVersion` does make version drift observable, which
    is the D18 pattern.
  - **Databricks** account credential configurations (`sts_role.role_arn`) have no
    documented creation-time validation and no `skip_validation` control, so a bad ARN
    surfaces at first use. Note the counter-example inside the same product: Unity Catalog
    storage credentials **do** validate at creation, defaulting `skip_validation` to
    `false` and offering an explicit validate endpoint. The lesson is that it is a choice,
    and Databricks made it both ways.

  Recommendation: validate a `LocationClass` at registration — resolve `parametersRef`,
  check the substrate version, confirm the controller identity can actually reach the
  account — and report the result on `Accepted` and `SupportedVersion` (D18). This is the
  behaviour Q8 says is landed in the type and absent from the code.

### Implementation Notes

**For api-dev:**

> **Amended by D10 through D15.** These notes predate the review. `LocationBinding` is a
> projected `Location`; `locationClassName` is `spec.locationClassRef` and nothing else;
> there is no `LocationClassName` enum to add and no `operatedBy` field; the group is
> `locations.miloapis.com/v1alpha1`. The multicluster mechanics, the finalizer, the
> label-index requirement, and the three-gate check carry over unchanged. The
> `ownerProjectRef` invariant does **not** carry over cleanly and is open — see Q16.

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
