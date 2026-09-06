#!/bin/bash
# measure.sh — one row with CPU sampled on both boxes during the run.
#
#   bench/measure.sh <label> <server-mode> -- <client args...>
#   SRV="-grohold 0" bench/measure.sh sink-nohold sink -- -client-stack netstack -iface eno2 -vlan 2131 -conns 4 -bulk -sink
#
# Unlike bench.sh it waits for the client to report it is running before it
# samples, so a client that binds AF_XDP itself (-client-stack netstack, which
# bounces the link for ~10 s at startup) is measured over its steady state
# only. Cores are whole-machine busy time on each box over an 8 s window.
# The server runs here with the lab addressing below; extra server flags in
# $SRV. Kills the client afterwards.
S=${OUT:-/tmp/netstack-bench}; C=${CLIENT:-root@206.223.228.217}; mkdir -p "$S"
busy() { awk '/^cpu /{print $2+$3+$4+$6+$7+$8+$9}' /proc/stat; }
cbusy() { ssh -o BatchMode=yes $C "awk '/^cpu /{print \$2+\$3+\$4+\$6+\$7+\$8+\$9}' /proc/stat"; }
lbl=$1; mode=$2; shift 3
# Graceful stop first, and wait for it: go-afxdp restores the NAPI sysctls and
# IRQ steering only on a clean Close, and a SIGKILL leaves them behind.
pkill -INT -x netstack 2>/dev/null; for _ in $(seq 1 10); do pgrep -x netstack >/dev/null || break; sleep 1; done; pkill -9 -x netstack 2>/dev/null; sleep 1
setsid "${BIN:-$(dirname "$0")/../netstack}" -iface eno2 -vlan 2131 -ip 192.168.0.1/24 -port 8080 -mtu 1500 -mode $mode ${SRV:-} > $S/m-$lbl.log 2>&1 < /dev/null & disown
for w in $(seq 1 26); do sleep 1; grep -q listening $S/m-$lbl.log && break; done
ssh -o BatchMode=yes $C "rm -f /root/c.log; (setsid /root/netstack -client 192.168.0.1:8080 -duration 12s $* > /root/c.log 2>&1 < /dev/null &)"
for w in $(seq 1 40); do sleep 1; ssh -o BatchMode=yes $C "grep -q 'client:' /root/c.log" && break; done
pid=$(pgrep -x netstack | head -1); pt() { awk '{print $14+$15}' "/proc/$pid/stat" 2>/dev/null || echo 0; }
sleep 2; s0=$(busy); c0=$(cbusy); p0=$(pt); sleep 8; s1=$(busy); c1=$(cbusy); p1=$(pt)
for w in $(seq 1 30); do sleep 1; ssh -o BatchMode=yes $C "grep -q 'total over' /root/c.log" && break; done
ssh -o BatchMode=yes $C 'pkill -INT -x netstack; for _ in $(seq 1 10); do pgrep -x netstack >/dev/null || break; sleep 1; done; pkill -9 -x netstack; true'
r=$(ssh -o BatchMode=yes $C "grep -E 'total over|latency' /root/c.log" | sed 's/.*total over [0-9.]*s: //; s/ (.*//; s/.*latency/latency/' | tr '\n' ' ')
sc=$(awk -v a=$s0 -v b=$s1 'BEGIN{printf "%.2f",(b-a)/100/8}'); cc=$(awk -v a=$c0 -v b=$c1 'BEGIN{printf "%.2f",(b-a)/100/8}'); pc=$(awk -v a=$p0 -v b=$p1 'BEGIN{printf "%.2f",(b-a)/100/8}')
echo "$lbl | $r| server-cores $sc (process $pc) client-cores $cc"
