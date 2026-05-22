# Service Catalog: Product Overview

## What Is the Service Catalog?

The Service Catalog is Milo's central registry for managed services — the authoritative source of truth for what services exist on the platform, what they expose, and how they relate to each other.

Think of it as the front door for any team that wants to offer a service on Milo. Before a service can bill customers, enforce quotas, or appear in the Milo portal, it must be registered here. The Service Catalog doesn't run workloads or process traffic — it declares what exists so every downstream system (billing, quota, telemetry, the portal) can stay in sync without coordinating directly.

## Where It Fits in the Milo Platform

Milo is a platform where **service providers** — Datum Cloud's own teams and eventually third-party teams — publish managed services that **consumers** (end-user projects and organizations) can discover, enable, and use.

Without a shared registry, each downstream system invents its own list of services and metrics. Billing has one list. The portal has another. Quota has a third. These drift apart silently until a service appears on an invoice but not in the portal, or a quota rule references a metric that billing doesn't recognize.

The Service Catalog solves this by acting as the upstream source of truth. Every system reads from it. Providers write once; the platform propagates everywhere.

```
Provider registers a service in the Service Catalog
         ↓
Billing learns what metrics to charge for
Quota learns what limits to enforce
Portal knows what to display to consumers
IAM knows what roles to provision on enablement
```

## Core Resources

The Service Catalog is built around four resources. Understanding how they relate to each other is the key to understanding the catalog's data model.

**Service** — The identity record for a managed service. It has a canonical name (e.g., `compute.miloapis.com`), a display name, a description, and an owner. Everything else in the catalog references a Service. This is the record billing puts on invoices, the portal uses as the display name, and quota uses to scope limits.

**ServiceConfiguration** — The provider's declaration of what their service contributes to the platform: which metrics it emits, how those metrics map to billing, and what quota limits apply. Where Service answers "what is this?", ServiceConfiguration answers "what does this do and how does the platform charge for it?" Updating a ServiceConfiguration automatically propagates changes to billing and quota — no cross-team coordination needed.

**ServiceEntitlement** — The consumer-side record that a project has enabled a service. Creating one triggers the full provisioning chain: quota allocation, billing enrollment, IAM role provisioning, and automatic enablement of any declared dependencies. Deleting one reverses all of that cleanly.

**ServiceConsumer** — The provider-side mirror of a ServiceEntitlement. One is created for each consumer project that enables a service, giving providers visibility into their consumer base. For gated services, this is also where providers record their approval or denial decision.

The relationship in plain terms: a provider creates a **Service** and a **ServiceConfiguration**. When a consumer enables that service, a **ServiceEntitlement** is created in their project and a **ServiceConsumer** is created in the provider's project.

## Service Lifecycle

Every Service (and its ServiceConfiguration) moves through a four-stage lifecycle:

**Draft** — The service is being built or iterated on. It is not visible to consumers and cannot be enabled. Providers can freely change any field, including the canonical name.

**Published** — The service is live. It appears in the catalog, consumers can enable it, and the canonical name is permanently locked. Breaking changes (renaming, changing ownership) are never applied to a Published service — they ship as a new service instead. Cosmetic fields like display name and description can still be updated.

**Deprecated** — The service is winding down. Existing consumers continue working, but the service is hidden from new consumers and the catalog. Providers typically communicate a sunset timeline before deprecating.

**Retired** — The service is archived. No new enablements are allowed. Records are preserved for audit and billing history.

Transitions are forward-only. A Published service cannot be moved back to Draft. This ensures consumers and downstream systems never encounter unexpected state changes.

## What the Service Catalog Is Not

These boundaries matter when reasoning about where the catalog ends and other services begin:

- **Not a runtime.** The catalog serves no user traffic and processes no events. It declares what exists; other services act on it.
- **Not a billing system.** Prices, rates, and usage data live in the billing service. The catalog declares what metrics exist and how they route to billing — it doesn't compute charges.
- **Not a telemetry pipeline.** The catalog declares what resource types are monitored; it doesn't collect or store metrics.
- **Not IAM.** The catalog describes what services exist and who has enabled them. Access control, authentication, and authorization are IAM's responsibility.

## For Service Providers

A **service provider** is any team (internal or external) that wants to offer a managed service on Milo. From their perspective, the Service Catalog is where they:

**Register and describe their service.**
Providers create a Service record with a canonical name (e.g., `compute.miloapis.com`), display name, and description. This becomes the stable identity referenced by billing, quota, and the portal.

**Declare what their service measures and charges for.**
Providers publish a Service Configuration that describes every metric their service emits (e.g., CPU hours, API requests, storage GB) and how those metrics route to billing and quota. This replaces the need to coordinate separately with the billing and quota teams — publish the configuration once, and both systems update automatically.

**Control who can enable their service.**
Providers choose between self-service access (any consumer can enable immediately) or gated access (consumers request access, and the provider approves). Gated access is critical for early-access programs, regulated services, or invite-only betas.

**See who is using their service.**
Once consumers enable a service, the provider sees a record for each consumer. This gives providers visibility into their consumer base and lets them manage approval/denial for gated services.

**Manage their service lifecycle.**
Services move through the Draft → Published → Deprecated → Retired lifecycle described above. Transitions propagate to billing, quota, and the portal automatically.

## For Service Consumers

A **service consumer** is a project or organization that wants to use a service available on Milo. From their perspective, the Service Catalog is what powers the service catalog experience in the portal:

**Discover available services.**
Consumers browse Published services — what they're called, what they do, and what they cost. The Service Catalog is the data source for any service marketplace or catalog view in the portal.

**Enable services.**
When a consumer enables a service, a single action triggers the full provisioning chain: quota is allocated, billing enrollment begins, required IAM roles are provisioned, and any services the enabled service depends on are automatically enabled too.

**Understand dependencies.**
Some services depend on others. Enabling Compute might require Networking. The Service Catalog declares these dependencies, and the platform resolves them automatically — consumers don't need to enable prerequisites manually or know they exist.

**Request access to gated services.**
For services that require provider approval, consumers can request access with a message explaining their use case. The catalog tracks the pending request and notifies the consumer when access is granted or denied.

**Disable services cleanly.**
Disabling a service tears down quota allocations, billing enrollment, and auto-enabled dependencies in a single action, leaving no orphaned configuration behind.

## Example: Launching a Private Beta Service

This flow illustrates how the catalog's resources work together end-to-end.

A provider team wants to launch a new managed database service, but only wants to onboard a small group of early customers initially.

**1. Provider registers the service in Draft.**
The team creates a `Service` record for `database.miloapis.com` and a `ServiceConfiguration` declaring their storage and compute metrics. Everything is in Draft — it is not visible to anyone outside the provider team.

**2. Provider sets the enablement policy to gated.**
Before publishing, the provider marks the service as gated. Consumers will be able to request access, but the provider must approve each one manually.

**3. Provider publishes the service.**
The service moves to Published. The canonical name `database.miloapis.com` is now locked. The service appears in the catalog. Billing and quota systems are updated automatically.

**4. A consumer requests access.**
A consumer project opens the catalog, finds the database service, and submits an access request with a note describing their use case. Their `ServiceEntitlement` is created with a `PendingApproval` status — no quota is allocated and billing is not yet active.

**5. Provider reviews and approves.**
The provider sees a new `ServiceConsumer` record for the requesting project. They review the request and record their approval. The platform transitions the consumer's entitlement to `Active`, allocates quota, and begins billing enrollment.

**6. Consumer uses the service.**
From the consumer's perspective, the service is now enabled. They can provision resources against it within their allocated quota.

**7. Provider later opens to general availability.**
When the beta ends, the provider changes the enablement policy to self-service. New consumers can now enable the service without waiting for approval. Existing consumers are unaffected.
