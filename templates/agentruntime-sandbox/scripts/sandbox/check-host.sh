#!/usr/bin/env bash
set -euo pipefail

echo "host=$(uname -s) arch=$(uname -m)"
if [[ "${SANDBOX_REQUIRE_KVM:-false}" == "true" ]]; then
  [[ "$(uname -s)" == "Linux" ]] || { echo 'KVM is only supported on Linux' >&2; exit 1; }
  [[ -r /dev/kvm && -w /dev/kvm ]] || { echo '/dev/kvm must be readable and writable' >&2; exit 1; }
fi

if [[ "$(id -u)" == "0" && "${ALLOW_ROOT_SANDBOX_TESTS:-false}" != "true" ]]; then
  echo 'sandbox tests must run as a non-root user; set ALLOW_ROOT_SANDBOX_TESTS=true only in a dedicated runner' >&2
  exit 1
fi

state_dir="${SANDBOX_STATE_DIR:-.runtime/state}"
deps_dir="${SANDBOX_DEPS_DIR:-.runtime/deps}"
mkdir -p "$state_dir" "$deps_dir"
test -w "$state_dir" && test -w "$deps_dir"
echo "state_dir=$state_dir"
echo "deps_dir=$deps_dir"
