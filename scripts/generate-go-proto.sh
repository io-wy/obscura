#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

command -v protoc >/dev/null || { echo "protoc is required" >&2; exit 1; }
command -v protoc-gen-go >/dev/null || { echo "protoc-gen-go is required" >&2; exit 1; }
command -v protoc-gen-go-grpc >/dev/null || { echo "protoc-gen-go-grpc is required" >&2; exit 1; }

mkdir -p control/gen
protoc \
  -I api/proto \
  --go_out=control/gen \
  --go_opt=paths=source_relative \
  --go-grpc_out=control/gen \
  --go-grpc_opt=paths=source_relative \
  api/proto/agentruntime/v1/runtime.proto \
  api/proto/agentruntime/v1/guest.proto

echo "generated Go protobuf bindings under control/gen"
