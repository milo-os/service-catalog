# Enhancement: Service Enablement

**Status:** Draft for stakeholder review
**Scope:** Covers how consumers enable services within their projects, how the platform handles service dependencies, how providers control access to their services, and how providers gain visibility into their consumer base.

> **In one line.** Consumers can enable services in their projects with a single action; providers can see who's using their services and control who gets access.

---

## The problem

The service registry tells Milo what services exist and who provides them. What the platform still lacks is any record of which projects are actually *using* a service — and the workflows around getting that access.

This gap has real consequences today:

- Billing and quota have no reliable signal for which projects are enrolled in a service. They infer it from API traffic, which is both fragile and retroactive.
- Service providers have no visibility into their consumer base. They can't see who is using their service, reach out to customers before a breaking change, or manage an early access program.
- There's no path for a provider to require sign-off before granting access — useful for regulated services, early access programs, or invite-only betas.

This enhancement introduces service enablement as a first-class platform capability, with two new resources that close these gaps: `ServiceEntitlement` on the consumer side and `ServiceConsumer` on the provider side.

---

## Consumer experience

### Enabling a service

A project admin or billing admin enables a service by creating a `ServiceEntitlement` in their project. From a product perspective, this is the moment a consumer project "opts in" to a service — it signals intent to use it and triggers the platform to make that service available in the project.

Once enabled, the platform automatically provisions what the project needs: quota allocations, billing enrollment, and IAM roles that let project members interact with the service. The consumer doesn't have to wire any of that up manually.

```yaml
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceEntitlement
metadata:
  name: compute
  namespace: projects/my-project
spec:
  serviceRef:
    name: compute.miloapis.com
```

When provisioning completes the entitlement becomes `Active` and the service is ready to use.

### Dependencies are handled automatically

Many services depend on other services to function. Compute, for example, requires Networking. When a consumer enables Compute, the platform detects that Networking isn't enabled and enables it automatically — the consumer doesn't need to know about the dependency or manually enable it themselves.

Auto-enabled dependencies are tracked with an `origin` field so the platform knows they were enrolled as a side effect of another service, not by explicit consumer choice. This matters for two reasons:

**Cleanup.** If a consumer later disables Compute, the platform checks whether Networking is still needed by any other service. If not, it disables it automatically. Services that were brought in as dependencies leave when the service that needed them leaves.

**Protection.** If a consumer tries to manually disable Networking while Compute is still active, the platform blocks the action with a clear message explaining why. The consumer needs to disable Compute first.

Dependency resolution is recursive — if a dependency has its own dependencies, those are enrolled too.

### Disabling a service

A consumer disables a service by deleting their `ServiceEntitlement`. The platform tears down the service's quota allocations and billing enrollment for that project, and cleans up any auto-enabled dependencies that are no longer needed.

---

## Provider experience

### Seeing your consumers

Every time a consumer enables a service, the platform creates a `ServiceConsumer` record in the provider's project. This gives providers a native view of their consumer base — something they can list, watch, and build tooling on top of — without needing special platform access or cross-tenant visibility.

```yaml
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceConsumer
metadata:
  name: my-project
  namespace: projects/compute-platform
spec:
  service: compute.miloapis.com
  consumerProjectRef:
    name: my-project
status:
  phase: Active
  entitledAt: "2026-05-13T10:00:00Z"
```

Providers don't create or delete these records — the platform manages them automatically. The provider sees one `ServiceConsumer` per enrolled project, and the record stays alive for as long as the project has the service enabled.

### Controlling access with provider-gated enablement

By default, services are self-service: any project on the platform can enable them immediately. Providers can change this for services that aren't ready for open access.

A provider sets `enablementPolicy.mode: GatedByProvider` on their service to require approval before a consumer project can use it. This is the right model for:

- **Early access programs** — the service is available but not yet generally accessible
- **Regulated or sensitive services** — the provider needs to vet consumers before granting access
- **Invite-only betas** — the provider wants explicit control over who is in the program

When a consumer tries to enable a gated service, their `ServiceEntitlement` starts in `PendingApproval` rather than becoming immediately active. The consumer can include a message explaining their use case. The service remains inaccessible — no quota, no billing enrollment, no IAM roles — until the provider approves.

```yaml
spec:
  serviceRef:
    name: ml-platform.acme.com
  requestMessage: "Building a recommendation engine for our e-commerce platform."
```

The corresponding `ServiceConsumer` record in the provider's project is where the approval happens. The approval decision is the only field the provider manages on this record — the platform owns everything else. The provider approves or denies directly on that record:

```yaml
spec:
  approval:
    decision: Approved  # or Denied
    message: "Approved for early access program."
```

On approval, the platform activates the entitlement and provisions the service for the consumer's project. On denial, the entitlement is marked `Rejected` and the consumer is notified. If a consumer wants to request access again after being denied, they delete their `Rejected` entitlement and create a new one — the platform cleans up the corresponding `ServiceConsumer`, resetting the process for the provider.

If a gated service has dependencies that are also gated, the entire enablement waits until all dependencies are approved. The parent entitlement stays in `PendingApproval` until every gated dependency in the chain is resolved.

---

## What this unlocks

- **Billing and quota have an enrollment signal.** Downstream systems read entitlements to determine which projects are enrolled in which services. No more inferring this from API traffic.
- **Providers know their consumers.** For the first time, a provider can list the projects using their service and see when they enrolled.
- **Early access programs are a first-class capability.** Providers don't need custom tooling or manual coordination to run a gated program — it's built into the service configuration.
- **Consumers don't have to manage dependencies.** The platform handles dependency enrollment and cleanup transparently.

## What this isn't

- **Not an access-control system.** Whether a project member can call a service's API is IAM's job. `ServiceEntitlement` is what tells IAM to provision roles for the project — it's the trigger, not the enforcer.
- **Not tenant-wide policy.** Enablement is per-project. An organization admin enabling a service across all projects under their tenant is a separate capability and is out of scope here.

## What comes later

- **Org-level defaults.** Let an organization admin mark a service as enabled by default for any new project created under their tenant.
- **Per-project configuration at enable time.** Some services may need consumer-provided configuration when enabling — region preferences, tier selection, and so on.
- **Richer consumer context.** Let consumers attach metadata to their entitlement — team contact, use-case, cost center — useful for provider analytics and support routing.
- **Entitlement transfer.** Define what happens to entitlements when a project moves between organizations.

---

## References

- [`service-registry.md`](./service-registry.md) — `Service` identity that `ServiceEntitlement` references
- [`metering-definitions.md`](./metering-definitions.md) — meters that become active per-project on entitlement
- [`downstream-push-architecture.md`](./downstream-push-architecture.md) — how the services operator propagates configuration into billing and quota
