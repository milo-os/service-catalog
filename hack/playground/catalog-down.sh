#!/usr/bin/env bash
# Catalog overlay teardown for the agent-framework playground.
#
# Removes EXACTLY the resources THIS overlay installed, and nothing else — in
# particular it never touches the BASE tier (assistant/gateway/streamco/sink in
# the patch-playground namespace) so the two tiers stay independently
# removable. The cluster is never deleted.
#
#   catalog-down.sh --dry-run   list what WOULD be deleted (proof P8), no changes
#   catalog-down.sh             delete our resources
#
# Selection: part-of=agent-framework-playground is the PROGRAM-wide label the
# contract mandates — BASE carries it too (e.g. namespace/patch-playground), so
# it does NOT distinguish tiers. We therefore target the overlay's namespace
# and its singleton cluster-scoped infra (CRDs, roles, webhooks) BY NAME, and
# use the label only for instances of our own CRDs, which BASE cannot have.
#
# Env: KIND_CLUSTER (default test-infra); KUBECONFIG derived if unset.
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-test-infra}"
LABEL="app.kubernetes.io/part-of=agent-framework-playground"
OUR_NS="agent-framework-playground"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

# Singleton cluster-scoped resources this overlay owns, addressed by name.
OWNED_CRDS=(
  services.services.miloapis.com
  serviceconfigurations.services.miloapis.com
  serviceconsumers.services.miloapis.com
  serviceentitlements.services.miloapis.com
  serviceagents.services.miloapis.com
  serviceagentconfigurations.services.miloapis.com
  agentbindings.services.miloapis.com
)
OWNED_CLUSTERROLES=(capability-provider services-controller-manager)
OWNED_CLUSTERROLEBINDINGS=(capability-provider services-controller-manager)
OWNED_VALIDATINGWEBHOOKS=(services-validating)
OWNED_MUTATINGWEBHOOKS=(services-mutating)

# Instances of our own CRDs (cluster-scoped). BASE has no such CRDs, so a label
# match here can only be ours. Reclaimed by the CRD deletion regardless.
CR_KINDS=(
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

# add_if_present KIND NAME -> append "kind/name" to matched if it exists.
matched=()
add_if_present() {
  local ref
  ref="$(kubectl get "$1" "$2" -o name --ignore-not-found 2>/dev/null || true)"
  [[ -n "${ref}" ]] && matched+=("${ref}")
}

log "Resources owned by the catalog overlay"

add_if_present namespace "${OUR_NS}"
for n in "${OWNED_CRDS[@]}";              do add_if_present customresourcedefinition "${n}"; done
for n in "${OWNED_CLUSTERROLES[@]}";      do add_if_present clusterrole "${n}"; done
for n in "${OWNED_CLUSTERROLEBINDINGS[@]}"; do add_if_present clusterrolebinding "${n}"; done
for n in "${OWNED_VALIDATINGWEBHOOKS[@]}"; do add_if_present validatingwebhookconfiguration "${n}"; done
for n in "${OWNED_MUTATINGWEBHOOKS[@]}";  do add_if_present mutatingwebhookconfiguration "${n}"; done

# Our CRD instances, by label (safe — BASE lacks these CRDs).
for kind in "${CR_KINDS[@]}"; do
  while IFS= read -r ref; do
    [[ -n "${ref}" ]] && matched+=("${ref}")
  done < <(kubectl get "${kind}" -l "${LABEL}" -o name 2>/dev/null || true)
done

if [[ ${#matched[@]} -eq 0 ]]; then
  echo "  (none found)"
else
  printf '  %s\n' "${matched[@]}"
fi

if [[ ${DRY_RUN} -eq 1 ]]; then
  echo
  echo "DRY RUN — nothing deleted. The BASE namespace (patch-playground) is"
  echo "deliberately absent: it shares the program label but is not ours."
  exit 0
fi

if [[ ${#matched[@]} -eq 0 ]]; then
  exit 0
fi

# Namespace first (reclaims namespaced resources), then the CRDs (which cascade
# every catalog CR instance), then the remaining named cluster-scoped infra.
log "Deleting the overlay namespace"
kubectl delete namespace "${OUR_NS}" --ignore-not-found --wait=true

log "Deleting catalog CRDs (cascades all catalog CR instances)"
for n in "${OWNED_CRDS[@]}"; do
  kubectl delete customresourcedefinition "${n}" --ignore-not-found --wait=true
done

log "Deleting named cluster-scoped RBAC + webhooks"
for n in "${OWNED_CLUSTERROLES[@]}";        do kubectl delete clusterrole "${n}" --ignore-not-found; done
for n in "${OWNED_CLUSTERROLEBINDINGS[@]}"; do kubectl delete clusterrolebinding "${n}" --ignore-not-found; done
for n in "${OWNED_VALIDATINGWEBHOOKS[@]}";  do kubectl delete validatingwebhookconfiguration "${n}" --ignore-not-found; done
for n in "${OWNED_MUTATINGWEBHOOKS[@]}";    do kubectl delete mutatingwebhookconfiguration "${n}" --ignore-not-found; done

log "Teardown complete"
