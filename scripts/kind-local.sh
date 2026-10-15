#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
cluster=obscura-test
context=kind-obscura-test
tools_dir="$repo_root/.tools"
kubeconfig="$tools_dir/kind-kubeconfig"
key_file="$tools_dir/kind-api-key"
action="${1:-}"
[[ $# -eq 1 ]] || { echo 'Usage: scripts/kind-local.sh up|smoke|status|down' >&2; exit 2; }
command -v kind >/dev/null || { echo 'kind CLI is required' >&2; exit 1; }
command -v kubectl >/dev/null || { echo 'kubectl is required' >&2; exit 1; }
command -v python3 >/dev/null || { echo 'python3 is required' >&2; exit 1; }
command -v rg >/dev/null || { echo 'rg is required' >&2; exit 1; }
mkdir -p "$tools_dir"
chmod 700 "$tools_dir"
kubectl_test() { kubectl --kubeconfig "$kubeconfig" --context "$context" "$@"; }
case "$action" in
  up)
    docker info >/dev/null 2>&1 || { echo 'Docker daemon is unavailable' >&2; exit 1; }
    if kind get clusters | rg -qx "$cluster"; then
      [[ -f "$kubeconfig" ]] || { echo "Cluster $cluster exists without this project's kubeconfig; refusing to adopt it" >&2; exit 1; }
    else
      kind create cluster --name "$cluster" --image kindest/node:v1.35.0 --wait 120s --kubeconfig "$kubeconfig"
    fi
    chmod 600 "$kubeconfig"
    if kubectl_test -n obscura-system get crd sandboxes.sandbox.obscura.dev >/dev/null 2>&1; then
      active_sandboxes="$(kubectl_test -n obscura-system get sandboxes -o name)"
      [[ -z "$active_sandboxes" ]] || { echo 'Delete active test Sandboxes before rebuilding the in-memory runtime' >&2; exit 1; }
    fi
    if [[ "${OBSCURA_SKIP_BUILD:-0}" == 1 ]]; then
      docker image inspect obscura-control:dev obscura-devruntime:dev >/dev/null || { echo 'Both local images must exist when OBSCURA_SKIP_BUILD=1' >&2; exit 1; }
    else
      docker build --build-arg "GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}" -f control/Dockerfile -t obscura-control:dev .
      docker build --build-arg "GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}" --target devruntime -f control/Dockerfile -t obscura-devruntime:dev .
    fi
    kind load docker-image obscura-control:dev --name "$cluster"
    kind load docker-image obscura-devruntime:dev --name "$cluster"
    kubectl_test label nodes --all obscura.dev/runtime-node=true --overwrite
    kubectl_test apply -f deploy/base/namespace.yaml
    if [[ ! -f "$key_file" ]]; then
      python3 -c 'import secrets,sys; sys.stdout.write(secrets.token_urlsafe(32))' > "$key_file"
      chmod 600 "$key_file"
    fi
    kubectl_test -n obscura-system create secret generic obscura-gateway-key \
      --from-file="api-key=$key_file" --dry-run=client -o yaml | kubectl_test apply -f -
    kubectl_test apply -k deploy/crds
    kubectl_test apply -k deploy/overlays/kind
    kubectl_test apply -f deploy/examples/kind.yaml
    kubectl_test -n obscura-system rollout restart deployment/obscura-devruntime deployment/obscura-controller deployment/obscura-gateway
    kubectl_test -n obscura-system rollout status deployment/obscura-devruntime --timeout=120s
    kubectl_test -n obscura-system rollout status deployment/obscura-controller --timeout=120s
    kubectl_test -n obscura-system rollout status deployment/obscura-gateway --timeout=120s
    echo "Kind stack ready. Run: make kind-smoke"
    echo "kubectl --kubeconfig '$kubeconfig' --context '$context' -n obscura-system get pods"
    ;;
  smoke)
    [[ -f "$kubeconfig" && -f "$key_file" ]] || { echo 'Run make kind-up first' >&2; exit 1; }
    local_port="${OBSCURA_KIND_PORT:-19000}"
    kubectl --kubeconfig "$kubeconfig" --context "$context" -n obscura-system \
      port-forward svc/obscura-gateway "$local_port:9000" >"$tools_dir/kind-port-forward.log" 2>&1 &
    forward_pid=$!
    trap 'kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true' EXIT
    for attempt in {1..50}; do
      if curl -fsS "http://127.0.0.1:$local_port/healthz" >/dev/null; then break; fi
      kill -0 "$forward_pid" 2>/dev/null || { cat "$tools_dir/kind-port-forward.log" >&2; exit 1; }
      sleep 0.2
    done
    curl -fsS "http://127.0.0.1:$local_port/healthz" >/dev/null
    ( cd control && OBSCURA_API_KEY="$(tr -d '\n' < "$key_file")" go run ./cmd/devlab --mode=smoke --url="http://127.0.0.1:$local_port" )
    ;;
  status)
    [[ -f "$kubeconfig" ]] || { echo 'Run make kind-up first' >&2; exit 1; }
    kubectl_test -n obscura-system get deployment,pods,svc,sandboxruntimes,sandboxes
    ;;
  down)
    [[ -f "$kubeconfig" ]] || { echo 'This project has no Kind kubeconfig; refusing to delete a cluster' >&2; exit 1; }
    kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig"
    rm -f "$kubeconfig" "$key_file" "$tools_dir/kind-port-forward.log"
    ;;
  *) echo 'Usage: scripts/kind-local.sh up|smoke|status|down' >&2; exit 2 ;;
esac
