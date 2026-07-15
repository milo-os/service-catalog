#!/usr/bin/env bash
# Catalog overlay teardown for the agent-framework playground.
#
# Removes EXACTLY the resources this overlay installed — everything labeled
# app.kubernetes.io/part-of=agent-framework-playground — and nothing else. The
# shared test-infra cluster's other workloads (ipam/etcd/nats/chainsaw/…) are
# never touched, and the cluster is never deleted.
#
#   catalog-down.sh --dry-run   list what WOULD be deleted (proof P8), no changes
#   catalog-down.sh             delete our labeled resources
#
# Env: KIND_CLUSTER (default test-infra); KUBECONFIG derived if unset.
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-test-infra}"
LABEL="app.kubernetes.io/part-of=agent-framework-playground"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

# Cluster-scoped kinds this overlay creates or owns. Namespaced resources are
# reclaimed by deleting the labeled namespace, so they are not swept here.
CLUSTER_KINDS=(
  namespace
  customresourcedefinition
  clusterrole
  clusterrolebinding
  validatingwebhookconfiguration
  mutatingwebhookconfiguration
  service.services.miloapis.com
  serviceagent.services.miloapis.com
  serviceagentconfiguration.services.miloapis.com
  serviceentitlement.services.miloapis.com
  serviceconsumer.services.miloapis.com
  agentbinding.services.miloapis.com
)

log() { printf '\n==> %s\n' "$*"; }

if [[ -z "${KUBECONFIG:-}" ]]; then
  KUBECONFIG="$(mktemp -t ti-kubeconfig.XXXXXX)"
  kind get kubeconfig --name "${KIND_CLUSTER}" > "${KUBECONFIG}"
  export KUBECONFIG
fi

log "Resources labeled ${LABEL}"
matched=()
for kind in "${CLUSTER_KINDS[@]}"; do
  while IFS= read -r name; do
    [[ -n "${name}" ]] && matched+=("${kind}/${name#*/}")
  done < <(kubectl get "${kind}" -l "${LABEL}" -o name 2>/dev/null || true)
done

if [[ ${#matched[@]} -eq 0 ]]; then
  echo "  (none found)"
else
  printf '  %s\n' "${matched[@]}"
fi

if [[ ${DRY_RUN} -eq 1 ]]; then
  echo
  echo "DRY RUN — nothing deleted. Every line above carries the ${LABEL} label."
  echo "Compare against a pre-install baseline snapshot to confirm no foreign resource is listed."
  exit 0
fi

if [[ ${#matched[@]} -eq 0 ]]; then
  exit 0
fi

# Delete the namespace first (reclaims all namespaced resources), then the
# cluster-scoped resources. --wait keeps the teardown honest.
log "Deleting labeled namespace(s)"
kubectl delete namespace -l "${LABEL}" --ignore-not-found --wait=true

log "Deleting labeled cluster-scoped resources"
for kind in customresourcedefinition clusterrole clusterrolebinding \
            validatingwebhookconfiguration mutatingwebhookconfiguration \
            service.services.miloapis.com serviceagent.services.miloapis.com \
            serviceagentconfiguration.services.miloapis.com \
            serviceentitlement.services.miloapis.com \
            serviceconsumer.services.miloapis.com \
            agentbinding.services.miloapis.com; do
  kubectl delete "${kind}" -l "${LABEL}" --ignore-not-found --wait=true 2>/dev/null || true
done

log "Teardown complete"
