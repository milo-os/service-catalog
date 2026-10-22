#!/usr/bin/env bash
#
# Removes this suite's entitlement from each named project and waits until it
# is gone. measure-setup.sh creates the entitlements itself, so chainsaw's own
# cleanup does not know about them, and the Service cannot be deleted while
# they exist.
set -euo pipefail
kubeconfig="${E2E_MILO_KUBECONFIG:-../.kubeconfig}"
for project in "$@"; do
  kubectl --kubeconfig "${kubeconfig}" --context "${project}" delete \
    serviceentitlement/e2e-concurrent-enablement --ignore-not-found --wait --timeout=60s
done
