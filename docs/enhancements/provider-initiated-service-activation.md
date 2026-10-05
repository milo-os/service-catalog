# Provider-initiated service activation

**Status:** Implemented MVP

Service providers can activate a service while onboarding a consumer without
asking the consumer to create the entitlement manually. Consumer consent remains
the security boundary: the provider principal must have the
`services.miloapis.com/serviceentitlements.activate` permission on the target
project, granted either directly on that project or inherited from its
organization.

## API flow

The provider creates a `ServiceActivationRequest` in its own project control
plane:

```yaml
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceActivationRequest
metadata:
  name: acme-prod-compute
spec:
  serviceRef:
    name: compute
  consumerProjectRef:
    name: acme-prod
  requestMessage: Enabling Compute during managed onboarding.
```

Admission records the authenticated actor and verifies two independent facts:

1. the request is made from the project that owns the referenced Service; and
2. the actor has the custom `activate` permission in the consumer project.

The controller then creates the ordinary `ServiceEntitlement` in the consumer
project. The entitlement records the activation request, provider project, and
actor in `spec.providerActivation`. Dependency enrollment, quota, provisioning,
the provider-side `ServiceConsumer`, suspension, and teardown continue through
the existing entitlement lifecycle.

For a `GatedByProvider` service, creating the activation request is itself the
provider's approval; the controller does not require the provider to approve its
own activation a second time.

## Consent

The permission is intentionally separate from `serviceentitlements.create`.
Providers never receive general write access in a consumer control plane. A Milo
`PolicyBinding` grants the provider-activation role to a named provider user or
service account, selecting either:

- a Project for direct consent; or
- an Organization for policy inherited by its projects.

Removing the binding prevents new activation requests. It does not disable an
already active service; the consumer deletes the `ServiceEntitlement` through
the normal lifecycle to do that.

## One-shot lifecycle

An activation request is a one-shot command, not desired state. Once it has
created an entitlement it records that entitlement's UID and mirrors its outcome.
If the consumer later disables the service, the old request becomes `Disabled`
and does not recreate access. A provider needs a new, currently authorized
request to activate the service again.

If an entitlement already exists, the request reports the existing state and
does not replace its original provenance.
