#!/usr/bin/env bash
set -euo pipefail

state_dir="${SANDBOX_STATE_DIR:-.runtime/state}"
mkdir -p "$state_dir"
marker="$state_dir/smoke-$$"
trap 'rm -f "$marker"' EXIT

printf 'creating\n' > "$marker"
grep -qx 'creating' "$marker"
printf 'ready\nrunning\npaused\ndeleted\n' >> "$marker"
grep -q '^paused$' "$marker"
grep -q '^deleted$' "$marker"

if [[ "${SANDBOX_SMOKE_COMMAND:-}" != "" ]]; then
  # The adopter-provided command must be a single executable command and should
  # operate only on the isolated state directory.
  bash -c "$SANDBOX_SMOKE_COMMAND"
else
  echo 'No SANDBOX_SMOKE_COMMAND configured; lifecycle marker smoke test passed.'
fi
