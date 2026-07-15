#!/usr/bin/env bash
# Fetch a project's capability documents from the capability-provider adapter,
# as the assistant's HTTPCapabilitySource would (the published contract:
# GET /projects/{name}/capability-documents).
#
#   fetch-capabilities.sh [project]   (default: demo-project)
#
# demo-project returns the StreamCo document (with MCP endpoints rewritten onto
# the gateway); any other name returns [] — the isolation boundary (P5).
set -euo pipefail

PROJECT="${1:-demo-project}"
KIND_CLUSTER="${KIND_CLUSTER:-test-infra}"
NS="agent-framework-playground"
LOCAL_PORT="${LOCAL_PORT:-18080}"

if [[ -z "${KUBECONFIG:-}" ]]; then
  KUBECONFIG="$(mktemp -t ti-kubeconfig.XXXXXX)"
  kind get kubeconfig --name "${KIND_CLUSTER}" > "${KUBECONFIG}"
  export KUBECONFIG
fi

kubectl -n "${NS}" port-forward svc/capability-provider "${LOCAL_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" 2>/dev/null || true' EXIT

# Wait for the forward to accept connections.
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${LOCAL_PORT}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.2
done

echo "GET /projects/${PROJECT}/capability-documents"
curl -fsS "http://127.0.0.1:${LOCAL_PORT}/projects/${PROJECT}/capability-documents" \
  | { jq . 2>/dev/null || cat; }
