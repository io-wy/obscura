#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
for dev_tool in go cargo node npm python3 protoc; do
  command -v "$dev_tool" >/dev/null || { echo "Required tool missing: $dev_tool (see docs/testing.md)" >&2; exit 1; }
done
export PATH="$repo_root/.tools/bin:$PATH"
mkdir -p .tools/bin
GOBIN="$repo_root/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="$repo_root/.tools/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
npm ci --prefix guest/runner
make proto-generate
./scripts/generate-guest-proto.sh
./scripts/envtest-assets.sh
echo 'Ready: make test-mac or make dev-up'
