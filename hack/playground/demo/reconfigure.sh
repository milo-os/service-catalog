#!/usr/bin/env bash
# Live-reconfiguration proof (P3): apply ServiceAgentConfiguration v2 (a
# narrower toolSelector), watch the AgentBinding reproject to the new version,
# and show the capability-provider now serving the narrowed tool surface — the
# next chat turn can no longer call the dropped tools.
#
#   reconfigure.sh            apply v2 and show the narrowed capabilities
#   reconfigure.sh --revert   deprecate v2 (binding falls back to v1)
#   reconfigure.sh --unpublish delete the entitlement (binding pruned; empty)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAMPLES="${HERE}/../samples"
KIND_CLUSTER="${KIND_CLUSTER:-test-infra}"
NS="agent-framework-playground"
MODE="${1:-apply}"

if [[ -z "${KUBECONFIG:-}" ]]; then
  KUBECONFIG="$(mktemp -t ti-kubeconfig.XXXXXX)"
  kind get kubeconfig --name "${KIND_CLUSTER}" > "${KUBECONFIG}"
  export KUBECONFIG
fi

log() { printf '\n==> %s\n' "$*"; }

# Root-cluster gate changes (a new Published configuration) propagate on the
# ServiceEntitlement watch or the reconciler's periodic resync. Annotating the
# entitlement forces an immediate reconcile so the demo does not wait on the
# resync interval. The nonce is unique per call.
POKE=0
poke() {
  POKE=$((POKE + 1))
  kubectl annotate serviceentitlement streaming-streamco-example \
    "playground.internal/poke=$(date +%s).${POKE}.$$" --overwrite >/dev/null
}

wait_binding_version() { # expected-version desc
  local deadline=$((SECONDS + 90))
  until [[ "$(kubectl get agentbinding streamco-streaming-agent -o jsonpath='{.spec.configurationVersion}' 2>/dev/null || true)" == "$1" ]]; do
    (( SECONDS >= deadline )) && { echo "FAIL: timed out waiting for $2" >&2; exit 1; }
    sleep 2
  done
  printf '    ok: %s\n' "$2"
}

case "${MODE}" in
  apply)
    log "Applying ServiceAgentConfiguration v2 (narrower toolSelector: streams_list only)"
    kubectl apply -f "${SAMPLES}/21-serviceagentconfiguration-v2.yaml"
    poke
    wait_binding_version "2.0.0" "AgentBinding reprojected to v2.0.0"
    log "AgentBinding tool surface now:"
    kubectl get agentbinding streamco-streaming-agent \
      -o jsonpath='{.spec.tools.mcpServers[0].toolSelector.include}{"\n"}'
    log "Capabilities served for demo-project (note the narrowed tool list + gateway endpoint):"
    "${HERE}/fetch-capabilities.sh" demo-project
    ;;
  --revert)
    log "Deprecating v2 (forward-only phase machine) — binding falls back to v1"
    kubectl patch serviceagentconfiguration streamco-streaming-agent-v2 \
      --type=merge -p '{"spec":{"phase":"Deprecated"}}'
    poke
    wait_binding_version "1.0.0" "AgentBinding fell back to v1.0.0"
    "${HERE}/fetch-capabilities.sh" demo-project
    ;;
  --unpublish)
    log "Deleting the ServiceEntitlement — the AgentBinding is pruned"
    kubectl delete -f "${SAMPLES}/30-serviceentitlement.yaml" --wait=true
    unpublish_deadline=$((SECONDS + 90))
    until ! kubectl get agentbinding streamco-streaming-agent >/dev/null 2>&1; do
      (( SECONDS >= unpublish_deadline )) && { echo "FAIL: binding not pruned" >&2; exit 1; }
      sleep 2
    done
    printf '    ok: AgentBinding pruned\n'
    log "Capabilities served for demo-project (now empty):"
    "${HERE}/fetch-capabilities.sh" demo-project
    ;;
  *)
    echo "usage: reconfigure.sh [apply|--revert|--unpublish]" >&2
    exit 1
    ;;
esac
