#!/usr/bin/env bash
#
# Stands up the multi-project e2e environment: a kind cluster hosting a real
# Milo control plane, the operator wired to it, and three real Projects.
#
# config/overlays/e2e runs the operator with
# --enable-single-cluster-for-e2e-tests, where every project name resolves to
# one cluster and no project's control plane is separable from another's. Here
# the Milo multicluster provider discovers real Projects and engages each one's
# control plane over its own URL path, which is what makes "an unentitled
# project receives nothing" mean anything.
#
# Idempotent: safe to re-run against an existing cluster.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KUBECONFIG_OUT="${REPO_ROOT}/test/e2e-milo/.kubeconfig"

CLUSTER_NAME="sc-milo"
# The hosting cluster is addressed through its own kubeconfig rather than the
# ambient one. A developer's default kubeconfig is shared with everything else
# running on the machine, and a context can be rewritten out from under a run.
HOST_KUBECONFIG="${REPO_ROOT}/bin/e2e-milo/kind.kubeconfig"

# Flux installs the Milo and billing manifests from published OCI artifacts, the
# same way config/overlays/e2e installs the billing one. Both artifacts and
# their pins are declared in config/overlays/e2e-milo/flux.

CERT_MANAGER_VERSION="v1.16.2"
# Only the two controllers these manifests use. Flux's other controllers would
# be four more images to pull for nothing.
FLUX_VERSION="v2.8.2"
FLUX_COMPONENTS="source-controller,kustomize-controller"

# Built by the caller (`task e2e-milo:setup`).
SERVICES_IMAGE="${SERVICES_IMAGE:-ghcr.io/milo-os/service-catalog:dev}"

MILO_HOST="https://127.0.0.1:32460"
PROJECT_PATH="/apis/resourcemanager.miloapis.com/v1alpha1/projects"

# The provider project owning the source objects, and two consumer projects.
# The suites entitle only the first consumer. The second measures "unentitled",
# and the isolation suite entitles it at the end to show it was never inert.
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

log "installing Flux ${FLUX_VERSION}"
flux install --kubeconfig "${HOST_KUBECONFIG}" \
  --version "${FLUX_VERSION}" --components "${FLUX_COMPONENTS}" >/dev/null

# --- milo -------------------------------------------------------------------
#
# What this environment adds to the bundle is applied first and directly: those
# resources are its own, and the bundle's apiserver refers to them only by name.
# The bundle itself, with the pins and patches that compose it, is Flux's.

log "deploying Milo"
khost apply -k "${REPO_ROOT}/config/overlays/e2e-milo/milo/overlay"
khost -n milo-system rollout status statefulset/etcd --timeout=180s

khost apply -f "${REPO_ROOT}/config/overlays/e2e-milo/flux/milo.yaml"
# The Kustomization waits on what it applies, so ready here means the apiserver
# is rolled out with its certificate issued, not merely that it was applied.
khost -n flux-system wait kustomization/milo --for=condition=ready --timeout=300s

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

# The billing CRDs belong in Milo, not in the hosting cluster, so Flux gets an
# identity there, under the key name it looks for rather than the operator's
# `kubeconfig`.
#
# This kubeconfig embeds a CA where every other client here skips verification:
# kustomize-controller rejects insecure-skip-tls-verify unless started with
# --insecure-kubeconfig-tls, and loosening the controller is a worse trade than
# trusting the CA cert-manager already issued from. cert-manager publishes that
# CA alongside the serving certificate, which names the in-cluster DNS name
# below.
log "installing the billing CRDs into Milo"
milo_ca="$(khost -n milo-system get secret milo-apiserver-tls -o jsonpath='{.data.ca\.crt}')"
[ -n "${milo_ca}" ] || { echo "milo-apiserver-tls has no ca.crt" >&2; exit 1; }

khost -n flux-system create secret generic milo-kubeconfig \
  --from-file=value=/dev/stdin --dry-run=client -o yaml <<EOF | khost apply -f - >/dev/null
apiVersion: v1
kind: Config
current-context: milo
clusters:
  - name: milo
    cluster:
      server: https://milo-apiserver.milo-system.svc.cluster.local:6443
      certificate-authority-data: ${milo_ca}
users:
  - name: e2e-admin
    user:
      token: e2e-milo-admin-token
contexts:
  - name: milo
    context: {cluster: milo, user: e2e-admin}
EOF

khost apply -f "${REPO_ROOT}/config/overlays/e2e-milo/flux/billing-crds.yaml"
khost -n flux-system wait kustomization/billing-crds --for=condition=ready --timeout=300s

log "installing the remaining CRDs into Milo"
kroot apply -f "${REPO_ROOT}/config/overlays/e2e/ipclass-crd.yaml" >/dev/null
kroot apply -f "${REPO_ROOT}/config/overlays/e2e/locationbinding-crd.yaml" >/dev/null
kroot apply -f "${REPO_ROOT}/config/overlays/e2e/httpproxy-crd.yaml" >/dev/null
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
# were broken. Wait, and fail loudly here: a silent wait that never matches
# looks like one that did.
log "waiting for the operator to engage every project"
# The running pod is re-resolved every iteration. `logs deployment/...` picks an
# arbitrary pod, which during a restart can be the departing one, and the
# manager exits and restarts if it starts before a kind it indexes has reached
# discovery. The pod that engages is often not the one running when this wait
# began.
for p in "${PROJECTS[@]}"; do
  engaged=""
  for _ in $(seq 1 60); do
    manager_pod="$(khost -n services-system get pods -l control-plane=services \
      --field-selector=status.phase=Running \
      --sort-by=.metadata.creationTimestamp -o name | tail -1)"
    # Logs are captured before matching rather than piped into `grep -q`: under
    # `pipefail`, grep closes the pipe on its first match, kubectl dies of
    # SIGPIPE, and the successful match reads as a failed pipeline. The wait
    # then never ends, however long the thing it waits for has been true.
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
