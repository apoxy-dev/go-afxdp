#!/bin/bash
# A runnable demo of natlb: one client, two backends, real TCP, no real NIC.
#
#   sudo ./examples/natlb/demo.sh up      build the topology and start natlb
#   sudo ./examples/natlb/demo.sh curl    fetch the VIP from the client
#   sudo ./examples/natlb/demo.sh down    remove everything
#
# Needs root, iproute2, and python3 (the backends are python3 -m http.server).
# Runs from anywhere: it cd's to the repo root itself.
#
# Everything hangs off one bridge, because natlb hairpins: it receives on an
# interface and transmits back out the same one, so the client and the backends
# have to be on the same segment as the load balancer.
#
#                  ns nlb-cl            ns nlb-b1      ns nlb-b2
#                 10.0.0.1/24         10.0.0.21/24   10.0.0.22/24
#                  02:..:01             02:..:21       02:..:22
#                      |                    |              |
#                      +------- nlb-br -----+--------------+
#                                   |
#                           nlb-lb0 (host)  10.0.0.10   02:..:10
#                                   |
#                          natlb, AF_XDP on nlb-lb0
#                          VIP 203.0.113.10:80
#
# The client routes the VIP via 10.0.0.10, so its packets arrive addressed to
# the load balancer's MAC with the VIP still in the IP header. That is what
# natlb picks up.
#
# Everything it creates is named nlb-*, and teardown only removes names in that
# namespace, so it cannot delete an interface that was already on your machine.
# It does add 10.0.0.0/24 to the host through nlb-lb0: if you already use that
# prefix, change NET below.
set -euo pipefail

cd "$(dirname "$(readlink -f "$0")")/../.." # the repo root, for `go build ./examples/natlb`

BR=nlb-br
LB=nlb-lb0
NSS="nlb-cl nlb-b1 nlb-b2"
VIP=203.0.113.10
VPORT=80
NATIP=10.0.0.10
LBMAC=02:00:00:00:00:10
BIN=${BIN:-/tmp/natlb}

# State lives in a root-owned directory. /tmp will not do: down() feeds this
# file to kill as root, and /tmp is world-writable, so anyone on the box could
# pre-create it and choose what root kills.
RUNDIR=${XDG_RUNTIME_DIR:-/run}/natlb-demo
PIDFILE=$RUNDIR/pids
LOG=$RUNDIR/natlb.log

need_root() { [ "$(id -u)" = 0 ] || { echo "run me with sudo" >&2; exit 1; }; }

mkns() { # name ip mac
        local ns=$1 ip=$2 mac=$3 veth="v-$1" br="vb-$1"
        ip netns add "$ns"
        ip link add "$veth" type veth peer name "$br"
        ip link set "$veth" netns "$ns"
        ip link set "$br" master $BR up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "$veth" address "$mac"
        ip netns exec "$ns" ip addr add "$ip/24" dev "$veth"
        ip netns exec "$ns" ip link set "$veth" up
}

up() {
        need_root
        down >/dev/null 2>&1 || true
        mkdir -p "$RUNDIR" && chmod 700 "$RUNDIR"

        echo "building the topology..."
        ip link add $BR type bridge
        ip link set $BR up

        # The load balancer's own port into the segment.
        ip link add $LB type veth peer name vb-$LB
        ip link set vb-$LB master $BR up
        ip link set $LB address $LBMAC
        ip addr add $NATIP/24 dev $LB
        ip link set $LB up

        mkns nlb-cl 10.0.0.1 02:00:00:00:00:01
        mkns nlb-b1 10.0.0.21 02:00:00:00:00:21
        mkns nlb-b2 10.0.0.22 02:00:00:00:00:22

        # The client reaches the VIP through the load balancer.
        ip netns exec nlb-cl ip route add $VIP/32 via $NATIP dev v-nlb-cl

        echo "starting the backends..."
        for b in nlb-b1 nlb-b2; do
                mkdir -p "$RUNDIR/$b"
                echo "hello from ${b#nlb-}" > "$RUNDIR/$b/index.html"
                ip netns exec $b python3 -m http.server $VPORT \
                        --directory "$RUNDIR/$b" >/dev/null 2>&1 &
                echo $! >> $PIDFILE
        done

        echo "building natlb..."
        go build -o "$BIN" ./examples/natlb

        echo "starting natlb on $LB..."
        "$BIN" -iface $LB \
                -vip $VIP:$VPORT \
                -nat-ip $NATIP \
                -backend 10.0.0.21:$VPORT@02:00:00:00:00:21 \
                -backend 10.0.0.22:$VPORT@02:00:00:00:00:22 \
                > "$LOG" 2>&1 &
        echo $! >> $PIDFILE
        sleep 3
        echo
        echo "ready. natlb's output goes to $LOG"
        echo "now run:  sudo $0 curl"
}

do_curl() {
        need_root
        echo "fetching http://$VIP/ from the client namespace, ten times:"
        for _ in $(seq 10); do
                ip netns exec nlb-cl curl -s --max-time 5 "http://$VIP/" || echo "(failed)"
        done
        # natlb reports once a second; wait for the second we just filled.
        sleep 2
        echo
        echo "--- natlb, the seconds it was actually working ---"
        grep "rx [1-9]" "$LOG" 2>/dev/null | tail -3 || echo "(no traffic reached natlb; see $LOG)"
}

down() {
        need_root
        # Only ever kill PIDs we wrote, and only if they really are PIDs.
        if [ -f "$PIDFILE" ]; then
                while read -r pid; do
                        case $pid in
                        '' | *[!0-9]*) continue ;; # not a pid; ignore it
                        esac
                        kill "$pid" 2>/dev/null || true
                done < "$PIDFILE"
        fi

        for ns in $NSS; do
                ip netns del "$ns" 2>/dev/null || true
        done
        # A namespace is not destroyed until the processes holding it exit, and
        # its veths outlive it by a fraction of a second. `up` gets back here in
        # about 50ms, so without this wait the rebuild fails with "File exists".
        for _ in $(seq 30); do
                still=0
                for ns in $NSS; do
                        ip link show "vb-$ns" >/dev/null 2>&1 && still=1
                done
                [ $still = 0 ] && break
                sleep 0.1
        done

        ip link del $LB 2>/dev/null || true
        ip link del $BR 2>/dev/null || true
        rm -rf "$RUNDIR"
        echo "cleaned up"
}

case "${1:-}" in
up) up ;;
curl) do_curl ;;
down) down ;;
*) echo "usage: $0 {up|curl|down}" >&2; exit 1 ;;
esac
