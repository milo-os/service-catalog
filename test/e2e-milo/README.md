# Multi-project e2e: the operator against real Milo

`test/e2e` runs the operator with `--enable-single-cluster-for-e2e-tests`, where
every project name resolves to the one kind cluster. That is enough to exercise
the reconcilers against a real API server, and it is what the suites there use.
It cannot say anything about isolation: with one control plane standing in for
all of them, "the unentitled project received nothing" and "the entitled project
received something" are claims about the same storage.

This environment exists for the claims the other one cannot make. It runs a real
Milo control plane and three real `Project`s, and the operator runs with the Milo
multicluster provider — no single-cluster flag. Each project is addressed at
`/apis/resourcemanager.miloapis.com/v1alpha1/projects/<id>/control-plane`, and
Milo routes each path to its own etcd prefix, so the projects are separate
storage reached over separate paths.

## Running it

```
task e2e-milo:setup   # test-infra cluster, Milo, CRDs, projects, the operator
task e2e-milo         # the chainsaw suites
task e2e-milo:down    # delete the cluster
```

`setup` brings up the same test-infra cluster every other environment in this
repo uses, and adds Milo to it: `config/overlays/e2e-milo/milo` composes the
published apiserver manifests with this environment's storage, credentials and
certificates, and attaches Milo to the Gateway test-infra already runs. Because
that is one kustomization rather than an artifact plus a patch set, it is
applied with `kubectl apply -k` and nothing here needs Flux.

It is the same `services-system` as `config/overlays/e2e`, so the two
environments are mutually exclusive on one machine — whichever was set up last
owns the operator. In CI they run on separate runners.

`test/e2e-milo/kubeconfig.yaml` is committed, with one context per addressable
control plane (`milo-root` and one per project); the chainsaw configuration
names them as clusters and each operation selects the one it means. Nothing in
it is discovered at run time — Milo is reached at the gateway's host port, a
project control plane is the same Milo behind its own URL path, and the token is
the static one `config/overlays/e2e-milo/milo/auth.yaml` installs.

## What is real here, and what is not

**Real:** the API server, per-project request routing, per-project storage,
project discovery and engagement by the Milo provider, admission webhooks
(served by the operator, called by Milo), and every reconciler's own loop.

**Not real:**

- **Per-project authorization.** Milo runs RBAC-only here (no OpenFGA
  authorization provider), and under RBAC a root grant authorizes every project
  path. Both identities in the static token file are in `system:masters`. So no
  suite here may claim that a caller was *refused* access to another project's
  plane — the isolation demonstrated is routing and storage. Note the operator's
  own identity is `system:masters` in production too, which is why
  `ProvisioningReconciler` re-checks the allowlist in code rather than relying on
  RBAC to bound it.
- **`Project.status.Ready`.** No Milo controller-manager is deployed, so the
  bootstrap sets it. The provider engages only Ready projects, so this is what
  makes a project reachable at all.
- **The IPClass CRD.** The permissive fixture from `config/overlays/e2e`, not the
  real IPAM API. It performs none of the authorization on `spec.source` that the
  ledger's authorization gap is about, so that gap is asserted from the ledger
  rather than observed from a refusal.
- **TLS trust.** Clients skip verification of Milo's serving certificate. The
  certificate is real and issued by cert-manager; nothing here is testing PKI.

## Guarding against a vacuous pass

"The unentitled project received nothing" passes for free against a plane that
is unreachable, or that does not serve the kind, or that answers every list with
nothing. Every absence assertion in `service-provisioning` is therefore paired
with a control:

1. Before anything is claimed about it, the suite creates an IPClass directly in
   the unentitled project and asserts it is there — so the plane is demonstrably
   reachable, serving that kind, and returning objects from a list.
2. That object carries none of the provisioning labels, so it is never a pruning
   candidate, and its survival through every step is itself an assertion that
   nothing done to the entitled project reached the other one.
3. At the end the second project is entitled and shown to receive the
   projection. A plane that can receive is a plane whose earlier emptiness meant
   something.

## Which suites live where

The three provisioning suites moved here, because the property they are about is
the one the single-cluster environment cannot show. Their assertions came with
them unchanged; what changed is that each operation now names the control plane
it addresses, and the provider-side ones (the source classes, the ServiceConsumer
a provider approves on) address the provider's project rather than a shared
cluster.

`test/e2e` keeps `service-lifecycle`, `serviceconfiguration-lifecycle` and
`project-suspension-propagation`, and the single-cluster mode they depend on is
unchanged.
