#!/usr/bin/env bash
set -euo pipefail

workspace="${GITHUB_WORKSPACE:-$(pwd)}"
workspace="$(cd "$workspace" && pwd -P)"
runtime_root="$workspace/.runtime"

normalize_runtime_path() {
  case "$1" in
    .runtime/*) printf '%s/%s\n' "$workspace" "$1" ;;
    "$runtime_root"/*) printf '%s\n' "$1" ;;
    *) echo "Refusing path outside $runtime_root: $1" >&2; exit 1 ;;
  esac
}

state_dir="$(normalize_runtime_path "${SANDBOX_STATE_DIR:-$runtime_root/state}")"
deps_dir="$(normalize_runtime_path "${SANDBOX_DEPS_DIR:-$runtime_root/deps}")"

is_runtime_path() {
  case "$1" in
    "$runtime_root"/*) return 0 ;;
    *) return 1 ;;
  esac
}

is_runtime_path "$state_dir" || { echo "Refusing state path outside $runtime_root: $state_dir" >&2; exit 1; }
is_runtime_path "$deps_dir" || { echo "Refusing deps path outside $runtime_root: $deps_dir" >&2; exit 1; }

for path in "$state_dir" "$runtime_root/logs"; do
  if [[ -e "$path" ]]; then
    find "$path" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  fi
done

echo "Cleaned runtime state under $workspace/.runtime"
