# Service Enablement — Implementation Architecture

**Status:** Proposed design
**Scope:** How `ServiceEntitlement` and `ServiceConsumer` fit into Milo's virtual
project control plane, where they live in the API surface, how the services
operator manages them, and what the cross-resource lifecycle looks like.

> **In one line.** Both resources are cluster-scoped and project-isolated
> through Milo's etcd-prefix storage layer: `ServiceEntitlement` in the consumer
> project's key space, `ServiceConsumer` in the provider project's key space —
> the services operator bridges them using the multicluster-runtime Milo provider.

---

## Background: Milo's virtual project control plane

As of August 2025 Milo uses a **single, unified APIServer** to serve all
project-scoped API requests. There is no per-project APIServer or controller
manager. Three cooperating layers provide tenant isolation:

**URL routing.** A `ProjectRouter` HTTP filter intercepts requests matching
`.../projects/{id}/control-plane/...`, strips that prefix, rewrites the path to
a plain `/apis/...` form, and stashes the project ID on the Go request context.
This filter is installed on all three handler chains (control plane, CRD
apiextensions, aggregator).

**Storage isolation.** Every storage backend is wrapped with a `ProjectAwareDecorator`
(`internal/apiserver/storage/project/`). On every storage operation the decorator
calls `projectMux.pick(ctx)`, which routes to a per-project child storage instance
whose etcd key prefix is `/projects/{id}/`. Resources written through
`.../projects/my-project/control-plane/...` land at etcd keys like
`/projects/my-project/registry/services.miloapis.com/serviceentitlements/my-entitlement`.
Resources written through the root endpoint land in the root key space. The two
key spaces are completely disjoint — root-endpoint requests cannot observe
project-stored resources.

CRD *definitions* (`apiextensions.k8s.io/customresourcedefinitions`) are
explicitly excluded from this wrapping so they remain shared across all project
contexts.

**Discovery filtering.** The `DiscoveryContextFilter` reads CRD annotations
(`discovery.miloapis.com/parent-contexts`) and omits resources not tagged for
the current context. This is a hint for API discovery, not a storage enforcement
boundary.

**Implication for resource scope.** Because isolation is enforced at the etcd
layer rather than the namespace layer, both cluster-scoped and namespaced
resource types are equally project-isolated when accessed through a project
URL. `ServiceEntitlement` and `ServiceConsumer` are declared
`scope=Cluster` — the project identity comes from the storage key prefix, not
from `metadata.namespace`.

---

## Where the new resources live

| Resource | API group | Scope | Parent context | Storage key prefix |
|---|---|---|---|---|
| `Service` | `services.miloapis.com/v1alpha1` | Cluster | `Platform` | root |
| `MeterDefinition` | `services.miloapis.com/v1alpha1` | Cluster | `Platform` | root |
| `MonitoredResourceType` | `services.miloapis.com/v1alpha1` | Cluster | `Platform` | root |
| **`ServiceEntitlement`** | `services.miloapis.com/v1alpha1` | **Cluster** | **`Project`** | `/projects/{consumer-project}/` |
| **`ServiceConsumer`** | `services.miloapis.com/v1alpha1` | **Cluster** | **`Project`** | `/projects/{provider-project}/` |

### Why cluster-scoped

The existing catalog resources are cluster-scoped because their canonical names
must be globally unique and they carry no project ownership. `ServiceEntitlement`
and `ServiceConsumer` follow the same scoping convention. Project isolation is
not provided by Kubernetes namespaces — it is provided by the `ProjectAwareDecorator`
routing each request to the correct per-project etcd prefix based on the
project ID extracted from the URL.

Declaring these resources cluster-scoped also means the services operator (an
external controller-runtime binary) can work with them via the standard
cluster-scoped client interface. Scoping is managed by which project cluster
connection is used, not by setting `metadata.namespace`.

---

## API types

### ServiceEntitlement

A consumer project admin creates one `ServiceEntitlement` per service they want
to use. The object is written into the consumer project's virtual control plane.

```yaml
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceEntitlement
metadata:
  name: compute-miloapis-com          # conventional: serviceName slug
spec:
  serviceRef:
    name: compute.miloapis.com        # resolves to a cluster-scoped Service
  requestMessage: "optional — used when service is GatedByProvider"
status:
  phase: Active                       # PendingApproval | Active | Rejected
  origin: Direct                      # Direct | Dependency
  dependencyOf: ""                    # set when origin=Dependency
  conditions: []
  observedGeneration: 0
```

**Go type sketch:**

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Origin",type=string,JSONPath=`.status.origin`
type ServiceEntitlement struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ServiceEntitlementSpec   `json:"spec,omitempty"`
    Status ServiceEntitlementStatus `json:"status,omitempty"`
}

type ServiceEntitlementSpec struct {
    ServiceRef     ServiceRef `json:"serviceRef"`
    RequestMessage string     `json:"requestMessage,omitempty"`
}

type ServiceEntitlementStatus struct {
    Phase              EntitlementPhase   `json:"phase,omitempty"`
    Origin             EntitlementOrigin  `json:"origin,omitempty"`
    DependencyOf       string             `json:"dependencyOf,omitempty"`
    EntitledAt         *metav1.Time       `json:"entitledAt,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// +kubebuilder:validation:Enum=PendingApproval;Active;Rejected
type EntitlementPhase string

const (
    EntitlementPhasePendingApproval EntitlementPhase = "PendingApproval"
    EntitlementPhaseActive          EntitlementPhase = "Active"
    EntitlementPhaseRejected        EntitlementPhase = "Rejected"
)

// +kubebuilder:validation:Enum=Direct;Dependency
type EntitlementOrigin string

const (
    EntitlementOriginDirect     EntitlementOrigin = "Direct"
    EntitlementOriginDependency EntitlementOrigin = "Dependency"
)
```

### ServiceConsumer

The services controller creates one `ServiceConsumer` in the provider project's
virtual control plane for every active (or pending) `ServiceEntitlement`.
Providers never create these; they only write the `spec.approval` field.

```yaml
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceConsumer
metadata:
  name: compute-miloapis-com--my-project   # {service-slug}--{consumer-project}
spec:
  serviceRef:
    name: compute.miloapis.com
  consumerProjectRef:
    name: my-project
  approval:                                # provider-writable; omitted for self-service
    decision: Approved                     # Approved | Denied
    message: "Approved for early access."
status:
  phase: Active                            # PendingApproval | Active | Denied
  entitledAt: "2026-05-13T10:00:00Z"
  conditions: []
  observedGeneration: 0
```

**Go type sketch:**

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Consumer",type=string,JSONPath=`.spec.consumerProjectRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type ServiceConsumer struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ServiceConsumerSpec   `json:"spec,omitempty"`
    Status ServiceConsumerStatus `json:"status,omitempty"`
}

type ServiceConsumerSpec struct {
    ServiceRef         ServiceRef         `json:"serviceRef"`
    ConsumerProjectRef ConsumerProjectRef `json:"consumerProjectRef"`
    Approval           *ProviderApproval  `json:"approval,omitempty"`
}

type ProviderApproval struct {
    // +kubebuilder:validation:Enum=Approved;Denied
    Decision ApprovalDecision `json:"decision"`
    Message  string           `json:"message,omitempty"`
}

type ServiceConsumerStatus struct {
    Phase              ConsumerPhase      `json:"phase,omitempty"`
    EntitledAt         *metav1.Time       `json:"entitledAt,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}
```

---

## Admission validation (webhooks)

Both types get validating webhooks in `internal/webhook/v1alpha1/`, consistent
with the existing webhook structure for `Service`, `MeterDefinition`, and
`MonitoredResourceType`.

### ServiceEntitlement webhook

- Reject if `spec.serviceRef.name` does not resolve to a `Published` `Service`.
- Reject `spec.serviceRef.name` changes after creation (immutable).
- Reject deletion if `status.origin == Dependency` and the parent entitlement
  is still `Active` (guard against manually removing a dependency mid-use).

### ServiceConsumer webhook

- Allow writes only from the services controller service account and from
  provider project members (for the `spec.approval` field only).
- Reject changes to any field other than `spec.approval` from non-controller
  callers.
- Reject changes to `spec.approval.decision` once set to `Denied` — re-requests
  must delete and recreate the `ServiceEntitlement`, which resets the flow.

---

## Controller design

### Multicluster-runtime wiring

The services operator (`cmd/services/main.go`) uses the `multicluster-runtime`
Milo provider to watch resources across all project virtual control planes.
This is the same pattern used by the quota subsystem in the Milo controller
manager.

```go
// 1. Standard controller-runtime manager connects to the Milo APIServer root.
mgr, _ := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme})

// 2. Milo provider watches Project resources and dynamically engages each
//    project's virtual control plane as a cluster.Cluster.
provider, _ := miloprovider.New(mgr, miloprovider.Options{
    InternalServiceDiscovery: false, // use Project resources + external URL
    ProjectRestConfig:        cfg,
})

// 3. Multicluster manager wraps the local manager and the provider.
mcMgr, _ := mcmanager.New(cfg, provider, mcmanager.Options{Scheme: scheme})

// 4. Register reconcilers — see below.
SetupReconcilers(mcMgr, provider)

// 5. Engage the root/"local" cluster (hosts Service, MeterDefinition, etc.).
mcMgr.Engage(ctx, "", mcMgr.GetLocalManager())

// 6. Start provider and mcMgr concurrently — each needs the other running,
//    so both launch in goroutines exactly as the quota system does.
go provider.Run(ctx, mcMgr)
go mcMgr.Start(ctx)
```

When a `Project` transitions to `Ready`, the provider constructs a
`*rest.Config` pointing at
`{miloAPIServer}/apis/resourcemanager.miloapis.com/v1alpha1/projects/{name}/control-plane`,
creates a `cluster.Cluster` from it (with a shared-informer cache), waits for
cache sync, and calls `mcMgr.Engage(ctx, projectName, cluster)`. The
reconcilers below then automatically begin watching that project.

### ServiceEntitlementReconciler

Registered with `WithEngageWithProviderClusters(true)` so it runs in every
engaged project cluster. Each reconcile call arrives with a
`ClusterAware` request carrying the project name as the cluster key — this is
the consumer project.

**On create/update:**

1. Use the current cluster's client (consumer project) to look up the
   `ServiceEntitlement` and the referenced `Service` from the root cluster
   (root cluster hosts cluster-scoped `Service` objects).
2. Reject (requeue with error) if the `Service` is not found or not `Published`.
3. Resolve `Service.spec.owner.producerProjectRef.name` to get the provider
   project name. Call `provider.Get(ctx, providerProjectName)` to obtain the
   provider project's `cluster.Cluster`.
4. Use the provider cluster's client to create or update the `ServiceConsumer`.
   Name convention: `{serviceSlug}--{consumerProject}`.
5. If `Service.spec.enablementPolicy.mode == SelfService` (the default):
   - Set `ServiceConsumer.status.phase = Active` via the provider cluster client.
   - Set `ServiceEntitlement.status.phase = Active` via the consumer cluster client.
   - Stamp `status.entitledAt`.
   - Emit downstream signals (billing enrollment, quota allocation — see
     [Downstream Push Architecture](./downstream-push-architecture.md)).
6. If `mode == GatedByProvider`:
   - Set both objects to `PendingApproval`.
   - Re-enqueue when `ServiceConsumer` is updated (provider sets approval).
7. Walk `Service.spec.dependencies[]` using the consumer cluster client:
   - For each dependency not already `Active`, create a `ServiceEntitlement` in
     the consumer project with `status.origin = Dependency` and
     `status.dependencyOf = <this name>`.
   - Dependency entitlements traverse the same reconcile path recursively.

**On delete (finalizer):**

1. Obtain the provider cluster via `provider.Get` and delete the corresponding
   `ServiceConsumer`.
2. Walk the dependency graph on the consumer cluster: for each entitlement where
   `status.dependencyOf == this.name` and no other active entitlement still
   requires that service, delete the dependency entitlement.
3. Emit billing/quota unenrollment signals.
4. Remove finalizer.

### ServiceConsumerReconciler

Also registered with `WithEngageWithProviderClusters(true)`. Each reconcile
call arrives in the context of the provider project cluster.

**On update (provider sets `spec.approval`):**

1. If `spec.approval.decision == Approved`:
   - Set `status.phase = Active` on the `ServiceConsumer`.
   - Obtain the consumer project cluster via
     `provider.Get(ctx, spec.consumerProjectRef.name)` and enqueue the
     corresponding `ServiceEntitlement` for re-reconciliation.
2. If `spec.approval.decision == Denied`:
   - Set `ServiceConsumer.status.phase = Denied`.
   - Obtain the consumer project cluster and set
     `ServiceEntitlement.status.phase = Rejected`.

---

## Dependency field on Service

To support automatic dependency enrollment, `ServiceSpec` gains two optional
fields:

```go
type ServiceSpec struct {
    // ... existing fields ...

    // Dependencies lists services that must be enabled alongside this service.
    // When a consumer enables this service the platform automatically enables
    // any listed dependency not already active in that project.
    //
    // +kubebuilder:validation:Optional
    // +kubebuilder:validation:MaxItems=16
    Dependencies []ServiceDependency `json:"dependencies,omitempty"`

    // EnablementPolicy controls whether consumers can self-service enable
    // this service or must wait for provider approval.
    //
    // +kubebuilder:validation:Optional
    EnablementPolicy *EnablementPolicy `json:"enablementPolicy,omitempty"`
}

type ServiceDependency struct {
    // +kubebuilder:validation:Required
    ServiceRef ServiceRef `json:"serviceRef"`
}

type EnablementPolicy struct {
    // +kubebuilder:validation:Enum=SelfService;GatedByProvider
    // +kubebuilder:default=SelfService
    Mode EnablementMode `json:"mode"`
}

type EnablementMode string

const (
    EnablementModeSelfService     EnablementMode = "SelfService"
    EnablementModeGatedByProvider EnablementMode = "GatedByProvider"
)
```

---

## Request flows

### Self-service enablement

```
Consumer admin
  └─ POST .../projects/my-project/control-plane
          /apis/services.miloapis.com/v1alpha1/serviceentitlements
     body: { spec.serviceRef.name: "compute.miloapis.com" }

Milo APIServer (webhook)
  └─ Validates: Service exists at root and is Published
  └─ Stores object under etcd prefix /projects/my-project/

ServiceEntitlementReconciler (engaged on "my-project" cluster)
  └─ Reads Service from root cluster → owner = "compute-platform"
  └─ provider.Get(ctx, "compute-platform") → provider cluster client
  └─ Creates ServiceConsumer "compute-miloapis-com--my-project"
     in compute-platform's virtual control plane
  └─ Sets both phases = Active, stamps entitledAt
  └─ Creates dependency ServiceEntitlement "networking-miloapis-com"
     in my-project (origin=Dependency, dependencyOf=compute-miloapis-com)
  └─ Signals billing: enroll my-project in compute.miloapis.com
```

### Gated enablement

```
Consumer admin
  └─ POST .../projects/my-project/control-plane/...
     body: { spec.serviceRef.name: "ml-platform.acme.com",
             spec.requestMessage: "Building recommendation engine" }

ServiceEntitlementReconciler
  └─ Service.enablementPolicy.mode = GatedByProvider
  └─ Creates ServiceConsumer in acme-platform virtual control plane
     (phase=PendingApproval)
  └─ Sets ServiceEntitlement.status.phase = PendingApproval
  └─ No billing/quota signals

Provider admin
  └─ PATCH .../projects/acme-platform/control-plane
           /apis/services.miloapis.com/v1alpha1
           /serviceconsumers/ml-platform-acme-com--my-project
     body: { spec.approval: { decision: Approved } }

ServiceConsumerReconciler (engaged on "acme-platform" cluster)
  └─ Sets ServiceConsumer.status.phase = Active
  └─ provider.Get(ctx, "my-project") → enqueues ServiceEntitlement

ServiceEntitlementReconciler (re-run on "my-project" cluster)
  └─ All gated dependencies resolved
  └─ Sets ServiceEntitlement.status.phase = Active
  └─ Signals billing/quota
```

### Disabling (delete)

```
Consumer admin
  └─ DELETE .../projects/my-project/control-plane/...
            /serviceentitlements/compute-miloapis-com

Webhook
  └─ Rejects if a Dependency entitlement still has this as dependencyOf

ServiceEntitlementReconciler (finalizer, "my-project" cluster)
  └─ provider.Get(ctx, "compute-platform") → deletes ServiceConsumer
  └─ Finds "networking-miloapis-com" (dependencyOf=compute-miloapis-com)
  └─ No other active entitlement requires networking → deletes it
  └─ Signals billing/quota: unenroll my-project from both services
  └─ Removes finalizer
```

---

## Discovery annotation placement

The `+kubebuilder:metadata:annotations` markers go on the Go type definitions
in `api/v1alpha1/`, consistent with the existing types:

```go
// ServiceEntitlement
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"

// ServiceConsumer
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
```

`task manifests` writes these annotations onto the generated CRD YAML. The
Milo APIServer's CRD informer picks them up within seconds and the discovery
filter begins scoping them to the project control-plane context.

---

## What this repo owns vs. what it does not

| Concern | Owner |
|---|---|
| `ServiceEntitlement` / `ServiceConsumer` CRD types | `services` (this repo) |
| Entitlement reconciler + dependency graph | `services` (this repo) |
| Consumer reconciler + approval flow | `services` (this repo) |
| Multicluster-runtime wiring + Milo provider | `services` (this repo) — same pattern as quota controllers in `milo` |
| Billing enrollment signal | `services` reconciler → `billing` (downstream push, see [architecture](./downstream-push-architecture.md)) |
| Quota allocation | `services` reconciler → `quota` (future — same push pattern) |
| IAM role provisioning | `services` reconciler → `iam` (future) |
| Provider-facing approval UX | Consumer of `ServiceConsumer` via project control plane |
| Org-level default enablement | Out of scope for this enhancement |

---

## Open questions

1. **Name convention for `ServiceConsumer`.** The name
   `{serviceName}--{consumerProject}` uses a double-dash delimiter to
   separate two reverse-DNS segments. Both segments use `.` internally, so
   `--` is unambiguous and survives Kubernetes name validation (253 chars max,
   DNS subdomain). Confirm this before implementation.

2. **Root cluster access from reconcilers.** The `ServiceEntitlementReconciler`
   needs to read cluster-scoped `Service` objects, which live in the root etcd
   key space (not in any project). Confirm the pattern for accessing the root
   cluster client from within a multicluster reconciler — likely
   `mcMgr.GetLocalManager().GetClient()`, consistent with how quota controllers
   access `ResourceRegistration` objects from the root cluster.

3. **Provider cluster unavailability.** If `provider.Get(ctx, providerProject)`
   returns an error because the provider project is not yet engaged (e.g. the
   project is newly created and the cache hasn't synced), the reconciler should
   requeue rather than fail permanently. Define the requeue backoff policy.

4. **Downstream signals.** The entitlement reconciler needs to notify billing
   and quota when a project activates or deactivates a service. The
   [Downstream Push Architecture](./downstream-push-architecture.md) doc covers
   the push pattern for `MeterDefinition`; service enablement signals should
   follow the same mechanism. Confirm whether billing needs a new event type or
   whether an existing push channel can carry the enrollment signal.

---

## References

- [PR #8 — Service Enablement Enhancement](https://github.com/milo-os/service-catalog/pull/8)
- [Service Registry](./service-registry.md)
- [Metering Definitions](./metering-definitions.md)
- [Downstream Push Architecture](./downstream-push-architecture.md)
- [Milo project storage decorator](../../fraud/tmp/milo/internal/apiserver/storage/project/)
- [Milo multicluster-runtime provider](../../fraud/tmp/milo/pkg/multicluster-runtime/milo/provider.go)
- [Discovery contexts doc](../../fraud/tmp/milo/docs/architecture/discovery-contexts.md)
