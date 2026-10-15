#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$repo_root/.tools/bin:$PATH"
dev_missing=0
echo "Host: $(uname -sm)"
for dev_tool in go cargo node npm python3 protoc protoc-gen-go protoc-gen-go-grpc; do
  if command -v "$dev_tool" >/dev/null; then
    echo "OK $dev_tool: $(command -v "$dev_tool")"
  else
    echo "MISSING $dev_tool"
    dev_missing=1
  fi
done
if [[ -n "${KUBEBUILDER_ASSETS:-}" ]]; then
  for dev_binary in kube-apiserver etcd kubectl; do
    if [[ -x "$KUBEBUILDER_ASSETS/$dev_binary" ]]; then echo "OK envtest $dev_binary";
    else echo "MISSING $KUBEBUILDER_ASSETS/$dev_binary"; dev_missing=1; fi
  done
else
  echo 'INFO envtest assets resolved by make dev-bootstrap / make dev-up (cached after first download)'
fi
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
  echo 'OPTIONAL Docker daemon available for image/Kind tests'
else
  echo 'OPTIONAL Docker daemon unavailable; native local lab does not need Docker'
fi
if [[ "$(uname -s)" == Linux && -r /dev/kvm && -w /dev/kvm ]]; then
  echo 'INFO /dev/kvm accessible; also verify Linux network tools, images and permissions'
else
  echo 'INFO real Rust node requires a Linux host with accessible /dev/kvm'
fi
exit "$dev_missing"
