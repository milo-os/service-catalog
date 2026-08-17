#!/usr/bin/env bash
#
# Stands up the multi-project e2e environment: a kind cluster hosting a real
# Milo control plane, the operator wired to it, and three real Projects.
#
# The difference from config/overlays/e2e is the whole point of it. There the
# operator runs with --enable-single-cluster-for-e2e-tests and every project
# name resolves to one cluster, so a project's control plane is not separable
# from any other's. Here the Milo multicluster provider discovers real Projects
# and engages each one's control plane over its own URL path, which is what
# makes "an unentitled project receives nothing" a statement about anything.
#
# Idempotent: safe to re-run against an existing cluster.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK_DIR="${REPO_ROOT}/bin/e2e-milo"
KUBECONFIG_OUT="${REPO_ROOT}/test/e2e-milo/.kubeconfig"

CLUSTER_NAME="sc-milo"
# The hosting cluster is addressed through its own kubeconfig rather than the
# ambient one. A developer's default kubeconfig is shared with everything else
# running on the machine, and a context can be rewritten out from under a run.
HOST_KUBECONFIG="${REPO_ROOT}/bin/e2e-milo/kind.kubeconfig"

# Milo. The bundle tag and the digest in
# config/overlays/e2e-milo/milo/root-kustomization.yaml must name the same
# release: the manifests and the binary they configure are one unit.
MILO_BUNDLE="ghcr.io/milo-os/milo-kustomize"
MILO_BUNDLE_TAG="v0.32.5"

# The same billing artifact config/overlays/e2e installs through Flux. Pulled
# directly because this environment runs no Flux.
BILLING_BUNDLE="ghcr.io/milo-os/billing-kustomize"
BILLING_BUNDLE_TAG="v0.0.0-main"

CERT_MANAGER_VERSION="v1.16.2"

# Built by the caller (`task e2e-milo:setup`).
SERVICES_IMAGE="${SERVICES_IMAGE:-ghcr.io/milo-os/service-catalog:dev}"

MILO_HOST="https://127.0.0.1:32460"
PROJECT_PATH="/apis/resourcemanager.miloapis.com/v1alpha1/projects"

# The provider project owning the source objects, and two consumer projects.
# Only the first consumer is entitled by the suites; the second is what
# "unentitled" is measured against, and is entitled at the end of the isolation
# suite to show it was never simply inert.
PROJECTS=(
  e2e-provisioning-platform
  e2e-provisioning-consumer-a
  e2e-provisioning-consumer-b
)

log() { echo "==> $*"; }

kroot() { kubectl --kubeconfig "${KUBECONFIG_OUT}" --context milo-root "$@"; }
khost() { kubectl --kubeconfig "${HOST_KUBECONFIG}" "$@"; }

# --- cluster ----------------------------------------------------------------

mkdir -p "$(dirname "${HOST_KUBECONFIG}")"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "kind cluster ${CLUSTER_NAME} already exists"
  kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${HOST_KUBECONFIG}"
else
  log "creating kind cluster ${CLUSTER_NAME}"
  kind create cluster --config "${REPO_ROOT}/hack/e2e-milo/kind-cluster.yaml" \
    --kubeconfig "${HOST_KUBECONFIG}" --wait 120s
fi

# Loaded here rather than by the caller: the cluster may not have existed a
# moment ago, and the operator Deployment below expects the tag present.
log "loading ${SERVICES_IMAGE}"
kind load docker-image "${SERVICES_IMAGE}" --name "${CLUSTER_NAME}"

log "installing cert-manager ${CERT_MANAGER_VERSION}"
khost apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null
khost -n cert-manager wait --for=condition=available deploy --all --timeout=300s

# --- milo -------------------------------------------------------------------

mkdir -p "${WORK_DIR}"
STAGE="${WORK_DIR}/milo-stage"
rm -rf "${STAGE}"
mkdir -p "${STAGE}/bundle"

log "fetching ${MILO_BUNDLE}:${MILO_BUNDLE_TAG}"
# `oras pull` skips layers with no title annotation, which is how Flux publishes
# these; fetch the layer blob and untar it.
digest="$(oras manifest fetch "${MILO_BUNDLE}:${MILO_BUNDLE_TAG}" | jq -r '.layers[0].digest')"
oras blob fetch "${MILO_BUNDLE}@${digest}" --output - | tar xz -C "${STAGE}/bundle"

cp -R "${REPO_ROOT}/config/overlays/e2e-milo/milo/overlay" "${STAGE}/overlay"
cp -R "${REPO_ROOT}/config/overlays/e2e-milo/milo/patches" "${STAGE}/patches"
cp "${REPO_ROOT}/config/overlays/e2e-milo/milo/root-kustomization.yaml" "${STAGE}/kustomization.yaml"

log "deploying Milo"
khost apply -k "${STAGE}"
khost -n milo-system rollout status statefulset/etcd --timeout=180s
khost -n milo-system rollout status deployment/milo-apiserver --timeout=300s

# --- host kubeconfig --------------------------------------------------------
#
# One context per addressable control plane. A project control plane is not a
# separate server: it is the same Milo behind a URL path, which is exactly what
# the suites need to address independently.

log "writing ${KUBECONFIG_OUT}"
mkdir -p "$(dirname "${KUBECONFIG_OUT}")"
{
  cat <<EOF
apiVersion: v1
kind: Config
current-context: milo-root
clusters:
  - name: milo-root
    cluster:
      server: ${MILO_HOST}
      insecure-skip-tls-verify: true
EOF
  for p in "${PROJECTS[@]}"; do
    cat <<EOF
  - name: ${p}
    cluster:
      server: ${MILO_HOST}${PROJECT_PATH}/${p}/control-plane
      insecure-skip-tls-verify: true
EOF
  done
  cat <<'EOF'
users:
  - name: e2e-admin
    user:
      token: e2e-milo-admin-token
contexts:
  - name: milo-root
    context: {cluster: milo-root, user: e2e-admin}
EOF
  for p in "${PROJECTS[@]}"; do
    echo "  - name: ${p}"
    echo "    context: {cluster: ${p}, user: e2e-admin}"
  done
} > "${KUBECONFIG_OUT}"

until kroot get --raw /readyz >/dev/null 2>&1; do sleep 2; done

# --- API surface in Milo ----------------------------------------------------

log "fetching ${BILLING_BUNDLE}:${BILLING_BUNDLE_TAG}"
BILLING_DIR="${WORK_DIR}/billing"
rm -rf "${BILLING_DIR}"
mkdir -p "${BILLING_DIR}"
digest="$(oras manifest fetch "${BILLING_BUNDLE}:${BILLING_BUNDLE_TAG}" | jq -r '.layers[0].digest')"
oras blob fetch "${BILLING_BUNDLE}@${digest}" --output - | tar xz -C "${BILLING_DIR}"

log "installing CRDs into Milo"
kroot apply -f "${BILLING_DIR}/base/crd/bases" >/dev/null
kroot apply -f "${REPO_ROOT}/config/overlays/e2e/ipclass-crd.yaml" >/dev/null
kroot apply -f "${REPO_ROOT}/config/overlays/e2e/locationbinding-crd.yaml" >/dev/null
kroot apply -k "${REPO_ROOT}/config/overlays/e2e-milo/milo-resources" >/dev/null

log "creating projects"
for p in "${PROJECTS[@]}"; do
  cat <<EOF | kroot apply -f - >/dev/null
apiVersion: resourcemanager.miloapis.com/v1alpha1
kind: Project
metadata:
  name: ${p}
spec:
  ownerRef:
    kind: Organization
    name: e2e-provisioning-org
EOF
  # Ready is set here because no Milo controller-manager is deployed. The
  # provider engages a project only once Ready is true, so this is what makes
  # the project's control plane reachable to the operator.
  kroot patch project "${p}" --type=merge --subresource=status \
    -p "{\"status\":{\"conditions\":[{\"type\":\"Ready\",\"status\":\"True\",\"reason\":\"E2EBootstrap\",\"message\":\"set by hack/e2e-milo/up.sh\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}" >/dev/null
done

# --- operator ---------------------------------------------------------------

log "deploying the operator"
khost create namespace services-system --dry-run=client -o yaml | khost apply -f - >/dev/null

khost -n services-system create secret generic milo-kubeconfig \
  --from-file=kubeconfig=/dev/stdin --dry-run=client -o yaml <<EOF | khost apply -f - >/dev/null
apiVersion: v1
kind: Config
current-context: milo
clusters:
  - name: milo
    cluster:
      server: https://milo-apiserver.milo-system.svc.cluster.local:6443
      insecure-skip-tls-verify: true
users:
  - name: services-operator
    user:
      token: e2e-milo-operator-token
contexts:
  - name: milo
    context: {cluster: milo, user: services-operator}
EOF

khost apply -k "${REPO_ROOT}/config/overlays/e2e-milo/operator"
khost -n services-system wait --for=condition=ready certificate/services-serving-cert --timeout=120s

# cert-manager's ca-injector only watches the cluster it runs in, and these
# webhook configurations live in Milo. Stamp the CA it issued.
log "injecting the webhook CA into Milo"
ca="$(khost -n services-system get secret services-webhook-cert -o jsonpath='{.data.ca\.crt}')"
[ -n "${ca}" ] || { echo "services-webhook-cert has no ca.crt" >&2; exit 1; }
for cfg in validatingwebhookconfigurations/services-validating mutatingwebhookconfigurations/services-mutating; do
  patch="$(kroot get "${cfg}" -o json |
    jq -c --arg ca "${ca}" '[.webhooks | to_entries[] |
      {op: "replace", path: "/webhooks/\(.key)/clientConfig/caBundle", value: $ca}]')"
  kroot patch "${cfg}" --type=json -p "${patch}" >/dev/null
done

# The manager exits at startup if a kind it indexes is not yet served, and CRD
# registration reaches discovery asynchronously. Restarting after the CRDs are
# in place is cheaper than making the manager tolerate it.
khost -n services-system rollout restart deployment/services-controller-manager >/dev/null
khost -n services-system rollout status deployment/services-controller-manager --timeout=180s

# A suite that runs before a project is engaged fails as though the operator
# were broken, so wait — and fail here rather than time out quietly, because a
# silent wait that never matches is indistinguishable from one that did.
log "waiting for the operator to engage every project"
# The running pod is re-resolved every iteration rather than named once.
# `logs deployment/...` picks an arbitrary pod, which during a restart can be
# the departing one; and the manager exits and is restarted if it starts before
# a kind it indexes has reached discovery, so the pod that eventually engages is
# often not the one running when this wait began.
for p in "${PROJECTS[@]}"; do
  engaged=""
  for _ in $(seq 1 60); do
    manager_pod="$(khost -n services-system get pods -l control-plane=services \
      --field-selector=status.phase=Running \
      --sort-by=.metadata.creationTimestamp -o name | tail -1)"
    # Logs are captured before matching rather than piped into `grep -q`: under
    # `pipefail`, grep closing the pipe on its first match kills kubectl with
    # SIGPIPE and the successful match reads as a failed pipeline — a wait that
    # never ends however long the thing it waits for has been true.
    manager_log="$([ -n "${manager_pod}" ] && khost -n services-system logs "${manager_pod}" --tail=-1 2>/dev/null || true)"
    if printf '%s' "${manager_log}" |
      grep -q "Successfully registered and engaged new cluster.*${p}"; then
      engaged=yes
      break
    fi
    sleep 2
  done
  [ -n "${engaged}" ] || {
    echo "project ${p} was never engaged by the operator" >&2
    khost -n services-system logs "${manager_pod}" --tail=100 >&2
    exit 1
  }
done

log "ready. KUBECONFIG=${KUBECONFIG_OUT}"
