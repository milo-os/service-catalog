#!/usr/bin/env bash
# Catalog overlay bring-up for the agent-framework playground (components 4-5).
#
# Builds the playground controller-manager + capability-provider images,
# kind-loads them into the shared test-infra cluster, installs the catalog
# CRDs + controllers + admission webhooks (via the cluster's cert-manager) and
# the capability-provider adapter, then applies the StreamCo demo CRs.
#
# This is the `--with-catalog` overlay for the playground: pg-infra's
# playground-up.sh calls it after BASE (assistant + gateway + StreamCo) is up.
# It is idempotent (P1) and additive — every resource is labeled
# part-of=agent-framework-playground. It NEVER touches the shared cluster's
# other workloads and NEVER runs `task cluster-down`.
#
# Env knobs:
#   KIND_CLUSTER            kind cluster name (default: test-infra)
#   KUBECONFIG              if unset, derived from `kind get kubeconfig`
#   MCP_GATEWAY_BASE_URL    Envoy AI Gateway MCPRoute base (overrides manifest)
#   SKIP_BUILD=1            skip docker build + kind load (reuse loaded images)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-test-infra}"
NS="agent-framework-playground"
OVERLAY="${REPO_ROOT}/config/overlays/playground"
SAMPLES="${REPO_ROOT}/hack/playground/samples"
DOCKERFILE="${REPO_ROOT}/hack/playground/Dockerfile"

MANAGER_IMAGE="ghcr.io/milo-os/service-catalog-playground-manager:dev"
ADAPTER_IMAGE="ghcr.io/milo-os/capability-provider:dev"

log()  { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# --- kubeconfig ------------------------------------------------------------
if [[ -z "${KUBECONFIG:-}" ]]; then
  KUBECONFIG="$(mktemp -t ti-kubeconfig.XXXXXX)"
  kind get kubeconfig --name "${KIND_CLUSTER}" > "${KUBECONFIG}" \
    || fail "could not get kubeconfig for kind cluster '${KIND_CLUSTER}' (is it up? run test-infra 'task cluster-up')"
  export KUBECONFIG
fi
kubectl get --raw='/healthz' >/dev/null 2>&1 || fail "cluster unreachable via KUBECONFIG=${KUBECONFIG}"

# --- preflight: never disturb the shared cluster ---------------------------
log "Preflight (shared cluster — additive installs only, never cluster-down)"
kubectl get ns cert-manager >/dev/null 2>&1 \
  || fail "cert-manager namespace not found — webhooks require it; is BASE bootstrapped?"
# Memory guardrail: abort if the node is already >80% memory-committed.
mem_pct="$(kubectl describe node 2>/dev/null \
  | awk '/Allocated resources/{f=1} f&&/memory/{gsub(/[()%]/,"",$3); print $3; exit}')"
if [[ -n "${mem_pct:-}" ]] && (( mem_pct > 80 )); then
  fail "node memory allocation ${mem_pct}% > 80% — aborting to protect the shared cluster's live workloads"
fi
note "cert-manager present; node memory allocation: ${mem_pct:-unknown}%"

# --- build + load images ---------------------------------------------------
if [[ "${SKIP_BUILD:-0}" != "1" ]]; then
  log "Building images"
  docker build -f "${DOCKERFILE}" --build-arg CMD=playground-manager  -t "${MANAGER_IMAGE}" "${REPO_ROOT}"
  docker build -f "${DOCKERFILE}" --build-arg CMD=capability-provider -t "${ADAPTER_IMAGE}" "${REPO_ROOT}"
  log "Loading images into kind cluster '${KIND_CLUSTER}'"
  kind load docker-image "${MANAGER_IMAGE}" --name "${KIND_CLUSTER}"
  kind load docker-image "${ADAPTER_IMAGE}" --name "${KIND_CLUSTER}"
else
  note "SKIP_BUILD=1 — reusing already-loaded images"
fi

# --- install overlay -------------------------------------------------------
log "Applying catalog overlay (CRDs + manager + webhooks + adapter)"
kubectl apply -k "${OVERLAY}"

if [[ -n "${MCP_GATEWAY_BASE_URL:-}" ]]; then
  log "Overriding MCP_GATEWAY_BASE_URL=${MCP_GATEWAY_BASE_URL}"
  kubectl -n "${NS}" set env deployment/capability-provider "MCP_GATEWAY_BASE_URL=${MCP_GATEWAY_BASE_URL}"
fi

log "Waiting for CRDs to establish"
for crd in services serviceagents serviceagentconfigurations serviceentitlements agentbindings; do
  kubectl wait --for=condition=Established "crd/${crd}.services.miloapis.com" --timeout=60s
done

log "Waiting for controller-manager + capability-provider to become Available"
kubectl -n "${NS}" rollout status deployment/services-controller-manager --timeout=180s
kubectl -n "${NS}" rollout status deployment/capability-provider --timeout=120s

# --- demo CRs (v1 active configuration) ------------------------------------
log "Applying StreamCo demo CRs (Service, Agent, Configuration v1, Entitlement)"
kubectl apply -f "${SAMPLES}/00-service.yaml"
kubectl apply -f "${SAMPLES}/10-serviceagent.yaml"
kubectl apply -f "${SAMPLES}/20-serviceagentconfiguration-v1.yaml"
kubectl apply -f "${SAMPLES}/30-serviceentitlement.yaml"

wait_json() { # resource name jsonpath expected desc
  local deadline=$((SECONDS + 120))
  until [[ "$(kubectl get "$1" "$2" -o jsonpath="$3" 2>/dev/null || true)" == "$4" ]]; do
    (( SECONDS >= deadline )) && fail "timed out waiting for $5"
    sleep 2
  done
  note "ok: $5"
}

log "Waiting for the projection chain to settle"
wait_json service.services.miloapis.com streaming-streamco-example \
  '{.status.conditions[?(@.type=="Ready")].status}' True "Service Ready"
wait_json serviceentitlement streaming-streamco-example \
  '{.status.phase}' Active "ServiceEntitlement Active"
wait_json agentbinding streamco-streaming-agent \
  '{.status.conditions[?(@.type=="Ready")].status}' True "AgentBinding Ready"

log "Catalog overlay is up"
kubectl -n "${NS}" get deploy,svc
echo
note "Fetch capabilities as a consumer:  hack/playground/demo/fetch-capabilities.sh demo-project"
note "Live reconfiguration proof (P3):    hack/playground/demo/reconfigure.sh"
note "Isolation proof (P5):               hack/playground/demo/fetch-capabilities.sh other-project"
