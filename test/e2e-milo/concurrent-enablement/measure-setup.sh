#!/usr/bin/env bash
#
# Enables e2e-concurrent-enablement in every project named on the command line
# at the same moment, and fails if any of them takes longer than
# SETUP_BUDGET_SECONDS to reach SetupComplete.
#
#   measure-setup.sh <label> <project>...
#
# Each project's time runs from immediately before its `kubectl create` to the
# watch event that shows SetupComplete=True, so it is what a user enabling the
# service would wait, including a client round trip at each end. The server-side
# setup record is printed alongside it for diagnosis; its timestamps have
# one-second resolution, which is too coarse to assert a sub-second budget on.
set -euo pipefail

label="$1"
shift

: "${SETUP_BUDGET_SECONDS:?SETUP_BUDGET_SECONDS must be set}"
# A path relative to this suite's directory, which is where chainsaw runs its
# scripts. Chainsaw hands a script a kubeconfig for one cluster only, and this
# script addresses several.
kubeconfig="${E2E_MILO_KUBECONFIG:-../.kubeconfig}"
[ -f "${kubeconfig}" ] || { echo "kubeconfig ${kubeconfig} not found; run 'task e2e-milo:setup'" >&2; exit 1; }

budget_ms=$((SETUP_BUDGET_SECONDS * 1000))
# The wait runs well past the budget so that an over-budget run still reports
# how long it took. "Did not finish in 3x the budget" is a different failure
# from "missed the budget by 200ms", and the message says which.
wait_seconds=$((SETUP_BUDGET_SECONDS * 3))

# A run interrupted before its cleanup leaves the entitlement behind, and
# creating over it would fail or, worse, time an entitlement that was set up
# long ago.
"$(dirname "$0")/delete-entitlements.sh" "$@" >/dev/null

results="$(mktemp -d)"
trap 'rm -rf "${results}"' EXIT

enable() {
  local project="$1"
  local start end
  start="$(date +%s%3N)"
  kubectl --kubeconfig "${kubeconfig}" --context "${project}" create -f entitlement.yaml >/dev/null
  if kubectl --kubeconfig "${kubeconfig}" --context "${project}" wait \
    serviceentitlement/e2e-concurrent-enablement \
    --for=condition=SetupComplete --timeout="${wait_seconds}s" >/dev/null; then
    end="$(date +%s%3N)"
    echo "$((end - start))" >"${results}/${project}"
  else
    echo "timeout" >"${results}/${project}"
  fi
}

pids=()
for project in "$@"; do
  enable "${project}" &
  pids+=("$!")
done
for pid in "${pids[@]}"; do
  wait "${pid}"
done

failed=""
echo "${label}: budget ${SETUP_BUDGET_SECONDS}s per project"
for project in "$@"; do
  elapsed="$(cat "${results}/${project}")"
  record="$(kubectl --kubeconfig "${kubeconfig}" --context "${project}" get \
    serviceentitlement/e2e-concurrent-enablement -o json |
    jq -r '.status.setup // {} |
      "requestedAt=\(.requestedAt // "-") authorizedAt=\(.authorizedAt // "-") firstCompletedAt=\(.firstCompletedAt // "-") stages=[\([.stages[]? | "\(.type)@\(.lastTransitionTime)"] | join(" "))]"')"
  if [ "${elapsed}" = "timeout" ]; then
    echo "  ${project}: did not reach SetupComplete within ${wait_seconds}s  ${record}"
    failed=yes
  elif [ "${elapsed}" -gt "${budget_ms}" ]; then
    echo "  ${project}: ${elapsed}ms, over budget  ${record}"
    failed=yes
  else
    echo "  ${project}: ${elapsed}ms  ${record}"
  fi
done

[ -z "${failed}" ] || {
  echo "${label}: setup exceeded the ${SETUP_BUDGET_SECONDS}s budget in at least one project" >&2
  exit 1
}
