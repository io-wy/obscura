#!/usr/bin/env bash
# Run a command with explicit local API server assets; never uses the current cluster.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/control"
envtest_version="${ENVTEST_K8S_VERSION:-1.35.0}"
envtest_tool='sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260305142021-f9589b9f2b9d'
if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
  envtest_setup="$repo_root/.tools/bin/setup-envtest"
  if [[ ! -x "$envtest_setup" ]]; then
    mkdir -p "$repo_root/.tools/bin"
    GOBIN="$repo_root/.tools/bin" go install "$envtest_tool"
  fi
  # --installed-only avoids network calls after the first setup.
  if envtest_assets="$("$envtest_setup" use --installed-only "$envtest_version" -p path 2>/dev/null)"; then
    export KUBEBUILDER_ASSETS="$envtest_assets"
  else
    echo "Preparing envtest $envtest_version for $(go env GOOS)/$(go env GOARCH)..." >&2
    envtest_assets="$("$envtest_setup" use "$envtest_version" -p path)"
    export KUBEBUILDER_ASSETS="$envtest_assets"
  fi
fi
for envtest_binary in kube-apiserver etcd kubectl; do
  if [[ ! -x "$KUBEBUILDER_ASSETS/$envtest_binary" ]]; then
    echo "Missing executable: $KUBEBUILDER_ASSETS/$envtest_binary" >&2
    echo 'Run make dev-bootstrap, or set KUBEBUILDER_ASSETS to an extracted envtest directory.' >&2
    exit 1
  fi
done
echo "Using envtest assets: $KUBEBUILDER_ASSETS" >&2
if [[ $# -gt 0 ]]; then
  exec "$@"
fi
