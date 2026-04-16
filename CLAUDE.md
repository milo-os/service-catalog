# Service Infrastructure

A Kubernetes-native service that hosts Milo's shared governance catalogs:
`Service`, `MeterDefinition`, and `MonitoredResourceType`. Built with CRDs,
controller-runtime, and admission webhooks. Patterned after the billing
service.

## Architecture

- **CRD-based**: kubebuilder v4 CRDs (not aggregated API server)
- **Controller-runtime**: reconcilers for lifecycle management
- **Webhooks**: validating (and minimal defaulting) webhooks for business rules
- **Cluster-scoped resources**: governance catalogs are global by design

## API Group

- Group: `services.miloapis.com`
- Version: `v1alpha1`
- Resources: `Service`, `MeterDefinition`, `MonitoredResourceType`

## Repo Layout

```
services/
├── cmd/services/main.go         # Binary entrypoint
├── api/v1alpha1/                # CRD type definitions
├── internal/
│   ├── config/                  # Operator configuration
│   ├── controller/              # Reconcilers + indexers
│   ├── validation/              # Validation logic
│   └── webhook/v1alpha1/        # Admission webhooks
├── config/                      # Kustomize manifests
├── hack/                        # Boilerplate
└── test/e2e/                    # Chainsaw E2E tests
```

## Key Design Decisions

1. **Cluster-scoped CRDs** — these are governance catalogs with globally
   unique canonical names (`spec.serviceName`, `spec.meterName`,
   `spec.resourceTypeName`); namespaces add ambiguity without buying
   isolation.
2. **Shared `Phase` lifecycle** — `Draft` → `Published` → `Deprecated` →
   `Retired`, applied uniformly to all three resources.
3. **Cross-resource references** — `MeterDefinition.spec.owner.service` and
   `MonitoredResourceType.spec.owner.service` resolve to a `Published`
   `Service.spec.serviceName`. Enforced at admission and re-checked at
   reconciliation.
4. **Self-service publishing** — providers create and publish their own
   resources; gating is left to RBAC and (later) review workflows.
5. **Finalizers for delete protection** — a `Service` cannot be deleted
   while meters or monitored resource types still reference it; meters and
   monitored resource types likewise block delete while downstream usage
   pipelines reference them.

## Reference Services

- **billing** (`go.miloapis.com/billing`) — primary pattern reference for
  CRD/webhook/controller structure and Taskfile/Dockerfile/main scaffolding.
  `MeterDefinition` is migrated from billing into this service.

## Verification Commands

```bash
task build         # Build binary
task test          # Run tests
task lint          # Run linter
task generate      # Run code generation
task manifests     # Generate CRD/RBAC/webhook manifests
```
