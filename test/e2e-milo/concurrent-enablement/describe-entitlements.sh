#!/usr/bin/env bash
#
# Prints this suite's entitlement in each named project, for the log of a
# failed run. The conditions and setup stages say which gate held it up.
kubeconfig="${E2E_MILO_KUBECONFIG:-../.kubeconfig}"
for project in "$@"; do
  echo "--- ${project}"
  kubectl --kubeconfig "${kubeconfig}" --context "${project}" get \
    serviceentitlement/e2e-concurrent-enablement -o yaml || true
done
