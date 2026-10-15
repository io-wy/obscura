#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]] || [[ "${EUID}" -ne 0 ]]; then
    echo "guest ext4 export requires root on Linux" >&2
    exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${1:?usage: build-guest-image.sh /absolute/path/rootfs.ext4}"
[[ "$output" = /* ]] || { echo "output must be an absolute path" >&2; exit 1; }
command -v docker >/dev/null
command -v mkfs.ext4 >/dev/null
command -v mount >/dev/null

image="obscura-guest:local"
docker build -f "$repo_root/guest/image/Dockerfile" -t "$image" "$repo_root"
container="$(docker create "$image")"
mountpoint="$(mktemp -d)"
mounted=0
cleanup() {
    if [[ "$mounted" -eq 1 ]]; then umount "$mountpoint"; fi
    rmdir "$mountpoint"
    docker rm "$container" >/dev/null
}
trap cleanup EXIT

truncate -s 2G "$output"
mkfs.ext4 -F -q "$output"
mount -o loop "$output" "$mountpoint"
mounted=1
docker export "$container" | tar -xpf - -C "$mountpoint" --numeric-owner
sync
echo "guest rootfs: $output"
