# services-ui

Remix SSR app for the Milo service catalog. Talks to the `services.miloapis.com`
CRDs (`Service`, `ServiceConfiguration`) on the kind dev cluster.

## Quick start

```bash
cd ui
pnpm dev
# → http://localhost:3000
```

That's the **primary dev workflow**. Vite serves with hot reload, source maps,
and React DevTools out of the box. No port-forward required.

### Preconditions

Both are usually already satisfied on a working dev box:

1. **Kind cluster is up** — `task test-infra:cluster-up` (from the repo root).
   This is what materializes `services/.test-infra/kubeconfig`, which the dev
   server reads to talk to the API.
2. **`node_modules` installed** — `pnpm install` (or use `task ui:dev` from the
   repo root, which installs as a dep before starting the server).

If `/services` shows a "Failed to load" alert, the cluster is probably down —
run `task test-infra:cluster-up` and restart the dev server.

## Credential resolution

`app/lib/kubeconfig.server.ts` picks the right credential path automatically:

1. **Env vars** (`SERVICES_API_SERVER_URL` etc.) — production / explicit override.
2. **In-cluster ServiceAccount** (`/var/run/secrets/...`) — when running inside
   the deployed pod.
3. **Local kubeconfig** at `../.test-infra/kubeconfig` — the dev fallback. This
   is what `pnpm dev` uses.

The kubeconfig is read once and cached at server start. If the kind cluster is
restarted (which rerolls the API server port), restart the dev server.

## When to use the deployed UI instead

For day-to-day development, prefer `pnpm dev`. The deployed `services-ui`
Deployment in the cluster exists for **verification only** — confirming that
the in-cluster ServiceAccount auth path works, that the manifests apply
correctly, and that RBAC is sufficient.

Two tasks back the deployed flow (run from the repo root):

- `task ui:deploy` — first-time deploy: build image, kind-load, `kubectl apply -k config/overlays/dev`.
- `task ui:redeploy` — rebuild, kind-load, restart the deployment, refresh the
  3001→3000 port-forward (logs in `/tmp/services-ui-port-forward.log`).

After `ui:redeploy`, the deployed UI is reachable at `http://localhost:3001`.

## Other tasks

```bash
pnpm type-check    # tsc --noEmit
pnpm lint          # currently aliases tsc --noEmit
pnpm test:e2e      # Playwright (expects dev server already running on :3000)
pnpm build         # production build
```

All are also exposed as `task ui:<name>` from the repo root.
