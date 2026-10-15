#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
mkdir -p guest/runner/src/gen
protoc -I api/proto \
    --plugin=protoc-gen-es=guest/runner/node_modules/.bin/protoc-gen-es \
    --es_out=guest/runner/src/gen \
    --es_opt=target=ts,import_extension=js \
    api/proto/agentruntime/v1/guest.proto
