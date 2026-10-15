#!/bin/sh
set -eu

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp

for option in $(cat /proc/cmdline); do
    case "$option" in
        obscura_id=*) export OBSCURA_SANDBOX_ID="${option#obscura_id=}" ;;
        ip=*)
            address="${option#ip=}"
            guest_ip="${address%%:*}"
            address="${address#*:}"
            address="${address#*:}"
            gateway_ip="${address%%:*}"
            ;;
    esac
done
: "${OBSCURA_SANDBOX_ID:?sandbox ID missing}"
: "${guest_ip:?guest IP missing}"
: "${gateway_ip:?gateway IP missing}"

ip link set lo up
ip address add "$guest_ip/30" dev eth0
ip link set eth0 up
ip route add default via "$gateway_ip" dev eth0

cd /opt/obscura
node dist/src/server.js &
runner_pid=$!
socat VSOCK-LISTEN:5000,reuseaddr,fork TCP:127.0.0.1:7000 &
proxy_pid=$!

trap 'kill "$runner_pid" "$proxy_pid" 2>/dev/null || true' EXIT TERM INT
wait "$runner_pid"
