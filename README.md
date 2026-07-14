# Service Catalog

Milo's central registry for managed services — the authoritative source of truth for what services exist on the platform, what they expose, and how they relate to each other. Consumed by billing, quota, telemetry, and the portal so each system stays aligned without re-inventing its own list.

## Documentation

- [Product overview](docs/overview.md) — what the service catalog is, how it fits into Milo, and what it enables for providers and consumers.
- [Enhancement proposals](docs/enhancements/) — design detail, spec shape, and worked examples for each resource.

## Development

Prerequisites: Go 1.25+, [Task](https://taskfile.dev), and a Kubernetes cluster (local or remote).

```bash
task build       # Build the binary
task test        # Run tests
task lint        # Run the linter
task manifests   # Regenerate CRD/RBAC/webhook manifests
task generate    # Regenerate deepcopy and other code
```

The repo uses a Go workspace (`go.work`) that includes the `billing` and `amberflo-provider` sibling modules so gopls and `go build` resolve cross-module references without needing published releases.

## Related repos

- **[milo-os/billing](https://github.com/milo-os/billing)** — billing service; consumes meter and resource type definitions pushed by this operator.
- **[milo-os/compute](https://github.com/milo-os/compute)** — compute service; one of the platform services registered here.
- **[milo-os/galactic](https://github.com/milo-os/galactic)** — networking service.
- **[milo-os/activity](https://github.com/milo-os/activity)** — human-readable activity timelines from control plane audit events.
