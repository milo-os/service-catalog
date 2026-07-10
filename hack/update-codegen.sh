#!/usr/bin/env bash
# Regenerates deepcopy, defaulters, and the typed clientset for the services
# API group. Tools are installed at pinned versions so the output is stable
# across machines; run this and commit the result whenever api/v1alpha1 changes.
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${SCRIPT_ROOT}"

CONTROLLER_TOOLS_VERSION="${CONTROLLER_TOOLS_VERSION:-v0.16.4}"
DEFAULTER_GEN_VERSION="${DEFAULTER_GEN_VERSION:-v0.32.3}"
CLIENT_GEN_VERSION="${CLIENT_GEN_VERSION:-v0.33.1}"

BIN="${SCRIPT_ROOT}/bin"
mkdir -p "${BIN}"

echo "Installing code generators..."
GOBIN="${BIN}" go install "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_TOOLS_VERSION}"
GOBIN="${BIN}" go install "k8s.io/code-generator/cmd/defaulter-gen@${DEFAULTER_GEN_VERSION}"
GOBIN="${BIN}" go install "k8s.io/code-generator/cmd/client-gen@${CLIENT_GEN_VERSION}"

echo "Generating deepcopy methods..."
"${BIN}/controller-gen" object:headerFile="hack/boilerplate.go.txt" paths="./..."

echo "Generating defaulters..."
"${BIN}/defaulter-gen" ./internal/config --output-file=zz_generated.defaults.go

# The typed clientset. The API lives in a flat api/v1alpha1 layout, so the group
# directory segment is derived from the parent dir ("api"); +groupGoName=Services
# in api/v1alpha1/doc.go keeps the Go names clean (client.ServicesV1alpha1()).
# Listers/informers are intentionally not generated: no controller consumes them.
echo "Generating typed clientset (services.miloapis.com/v1alpha1)..."
rm -rf "${SCRIPT_ROOT}/pkg/generated/clientset"
"${BIN}/client-gen" \
  --clientset-name versioned \
  --input-base "${SCRIPT_ROOT}" \
  --input api/v1alpha1 \
  --output-dir "${SCRIPT_ROOT}/pkg/generated/clientset" \
  --output-pkg go.miloapis.com/service-catalog/pkg/generated/clientset \
  --go-header-file hack/boilerplate.go.txt \
  --prefers-protobuf=false

echo "Code generation complete."
