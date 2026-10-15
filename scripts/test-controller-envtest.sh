#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "$repo_root/scripts/envtest-assets.sh" go test -tags=envtest ./internal/controller -run TestAPIValidationAndControllerRecovery -count=1
