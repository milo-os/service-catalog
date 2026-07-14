#!/usr/bin/env bash
# Fails if the committed generated code is out of date. Regenerates into the
# working tree and checks that nothing changed. Intended for CI.
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${SCRIPT_ROOT}"

bash "${SCRIPT_ROOT}/hack/update-codegen.sh"

if [ -n "$(git status --porcelain -- api pkg/generated)" ]; then
  echo ""
  echo "ERROR: generated code is out of date. Run 'hack/update-codegen.sh' and commit the result." >&2
  git --no-pager diff -- api pkg/generated >&2
  exit 1
fi

echo "Generated code is up to date."
