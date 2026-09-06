#!/bin/bash
# rig100.sh — one measurement row on a two-box rig driven from a third machine.
#
#   bench/rig100.sh <label> <server-mode> -- <client args...>
#   SRV="-busypoll 50" bench/rig100.sh sink-bp sink -- -conns 16 -bulk -sink
#   SRV="-stack kernel" bench/rig100.sh k-rr echo -- -conns 1024 -msg 64
#
# The server runs on $SERVER, the client on $CLIENT, both over ssh (sshpass
# with $SSHPASS, or keys). It waits for the client to report it is running,
# samples both boxes' whole-machine busy time and softirq share over an 8 s
# window of the steady state, then prints one line. Everything is confined to
# $IFACE; nothing here touches any other interface. Server and client are
# stopped with SIGINT and waited for (go-afxdp restores its sysctls only on a
# clean stop).
set -u
SERVER=${SERVER:-ubuntu@103.88.233.83}; CLIENT=${CLIENT:-ubuntu@103.88.233.93}
# The boxes' password comes from the environment, never from this file.
: "${SSHPASS:?set SSHPASS to the lab password, or switch SSH below to keys}"
IFACE=${IFACE:-eno2}; VLAN=${VLAN:-2043}; IP=${IP:-192.168.43.1/24}; PORT=${PORT:-8080}
TARGET=${TARGET:-192.168.43.1:8080}; DUR=${DUR:-12}; WIN=${WIN:-8}
BIN=${BIN:-/home/ubuntu/netstack}; OUT=${OUT:-/tmp/netstack-bench}
SSH="sshpass -e ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o BatchMode=no"  # -e: the password from SSHPASS, off the command line

lbl=$1; mode=$2; shift 3
srv() { $SSH "$SERVER" "$@"; }
cli() { $SSH "$CLIENT" "$@"; }
# busy ticks (all but idle+iowait) and softirq ticks, one line
stat() { awk '/^cpu /{print $2+$3+$4+$6+$7+$8+$9, $8}' /proc/stat; }
stat_cmd='awk "/^cpu /{print \$2+\$3+\$4+\$6+\$7+\$8+\$9, \$8}" /proc/stat'
stop() { # $1 = host role
  "$1" 'sudo pkill -INT -x netstack 2>/dev/null; for _ in $(seq 1 15); do pgrep -x netstack >/dev/null || break; sleep 1; done; sudo pkill -9 -x netstack 2>/dev/null; true'
}

stop srv; stop cli
# BUDGET=P confines the server to physical cores 0..P-1 (SMT siblings, which
# this rig lists as 24..47, stay idle): P queues, their interrupts steered to
# those cores, the process taskset to them. Both stacks get the same recipe;
# netstack runs with -affinity=false so go-afxdp does not place anything
# itself. Restored to $QUEUES queues and the saved interrupt masks afterwards.
QUEUES=${QUEUES:-16}
# setq N: change the channel count. mlx5 refuses to shrink below the RSS
# table's reach and to spread a table over channels that do not exist yet,
# so the table is shrunk before and spread after.
setq() { srv "cur=\$(sudo ethtool -l $IFACE | tail -1 | awk '{print \$2}'); if [ $1 -lt \$cur ]; then sudo ethtool -X $IFACE equal $1; fi; sudo ethtool -L $IFACE combined $1; sudo ethtool -X $IFACE equal $1"; }
confine() {
  [ -z "${BUDGET:-}" ] && return
  setq "$BUDGET"
  srv "mkdir -p $OUT/irq; \
    for i in \$(ls /sys/class/net/$IFACE/device/msi_irqs); do n=\$(grep -E \"^ *\$i:\" /proc/interrupts | grep -oE 'mlx5_comp[0-9]+' | grep -oE '[0-9]+$'); \
      [ -n \"\$n\" ] || continue; [ -f $OUT/irq/\$i ] || cat /proc/irq/\$i/smp_affinity_list > $OUT/irq/\$i; \
      if [ \"\$n\" -lt $BUDGET ]; then echo \$(( n + ${IRQ_BASE:-0} )) | sudo tee /proc/irq/\$i/smp_affinity_list >/dev/null; fi; done"
  TASKSET="taskset -c 0-$((BUDGET-1))"
  # queue n's interrupt is on core n (or IRQ_BASE+n, to take the driver's
  # softirq off the budget cores); its receive goroutine goes one core over,
  # so no worker sits on its own interrupt (that starves it).
  rot=""; for ((n=0; n<BUDGET; n++)); do rot="$rot,$(( (n+1) % BUDGET ))"; done
  SRV="${SRV:-} -affinity=false -cpus ${rot#,}"
}
release() {
  [ -z "${BUDGET:-}" ] && return
  srv "for f in $OUT/irq/*; do [ -f \$f ] || continue; i=\$(basename \$f); cat \$f | sudo tee /proc/irq/\$i/smp_affinity_list >/dev/null; done; rm -rf $OUT/irq"
  setq "$QUEUES"
}
TASKSET=""
# Released on every exit path: a server that fails to start, a Ctrl-C.
cleanup() { trap - EXIT INT TERM; stop srv 2>/dev/null; stop cli 2>/dev/null; release; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM
confine
srv "mkdir -p $OUT; sudo setsid $TASKSET $BIN -iface $IFACE -vlan $VLAN -ip $IP -port $PORT -mtu 1500 -mode $mode ${SRV:-} > $OUT/$lbl.log 2>&1 < /dev/null & disown"
for _ in $(seq 1 40); do sleep 1; srv "grep -q listening $OUT/$lbl.log" && break; done
srv "grep -q listening $OUT/$lbl.log" || { echo "$lbl | SERVER FAILED: $(srv "tail -2 $OUT/$lbl.log" | cut -c1-200)"; exit 1; }
sleep 2
cli "mkdir -p $OUT; rm -f $OUT/c-$lbl.log; sudo nstat -n >/dev/null; (sudo setsid $BIN -client $TARGET -duration ${DUR}s $* > $OUT/c-$lbl.log 2>&1 < /dev/null &)"
for _ in $(seq 1 45); do sleep 1; cli "grep -q 'client:' $OUT/c-$lbl.log" && break; done
sleep 2
s0=$(srv "$stat_cmd"); c0=$(cli "$stat_cmd"); sleep "$WIN"; s1=$(srv "$stat_cmd"); c1=$(cli "$stat_cmd")
for _ in $(seq 1 40); do sleep 1; cli "grep -q 'total over' $OUT/c-$lbl.log" && break; done
r=$(cli "grep -E 'total over|latency' $OUT/c-$lbl.log" | sed 's/.*total over [0-9.]*s: //; s/ (.*//; s/.*latency/latency/' | tr '\n' ' ')
nst=$(cli "nstat 2>/dev/null | grep -E '${NSTAT_RE:-TcpInCsumErrors|TcpRetransSegs}' | awk '{printf \"%s=%s \", \$1, \$2}'" | sed 's/Tcp//g')
cores() { echo "$1" "$2" | awk -v w="$WIN" '{printf "%.2f (softirq %.2f)", ($3-$1)/100/w, ($4-$2)/100/w}'; }
stop cli
echo "$lbl | $r| server $(cores "$s0" "$s1") client $(cores "$c0" "$c1") | client: $nst"
echo "   server: $(srv "grep -E 'offload:' $OUT/$lbl.log | tail -1 | sed 's/.*offload: //'" | cut -c1-160)"
stop srv
release
