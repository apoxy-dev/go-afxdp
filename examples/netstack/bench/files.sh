#!/bin/bash
# files.sh — the file-fetch comparison: a handicapped server against a client
# that uses the whole box.
#
#   bench/files.sh <label> <stack: kernel|kernel-sendfile|netstack|netstack-zc> -- [extra server flags]
#   BUDGET=4 WORKERS=2000 bench/files.sh k4 kernel
#
# The server is confined to BUDGET physical cores: that many NIC queues, their
# interrupts on those cores, and the process pinned to them (netstack also runs
# with -affinity=false and its workers pinned one core over from their own
# interrupt). The kernel gets exactly the same recipe, so both stacks are
# measured with the same machine. The client is unconstrained and dials with
# every core it has.
set -u
SERVER=${SERVER:-ubuntu@103.88.233.83}; CLIENT=${CLIENT:-ubuntu@103.88.233.93}
# The boxes' password comes from the environment, never from this file.
: "${SSHPASS:?set SSHPASS to the lab password, or switch SSH below to keys}"
IFACE=${IFACE:-eno2}; VLAN=${VLAN:-2043}; IP=${IP:-192.168.43.1/24}; PORT=${PORT:-8080}
CLIENT_IP=${CLIENT_IP:-192.168.43.2/24}; SERVER_MAC=${SERVER_MAC:-7c:c2:55:be:f4:e1}
TARGET=${TARGET:-192.168.43.1:8080}
BUDGET=${BUDGET:-4}; QUEUES=${QUEUES:-48}
WORKERS=${WORKERS:-2000}; PERCONN=${PERCONN:-4}
PORTS=${PORTS:-1}; SLOW=${SLOW:-}   # server ports from $PORT up; client pacing, e.g. 50k
MIX=${MIX:-64k:50,1m:30,5m:15,100m:5}
DUR=${DUR:-30}; WIN=${WIN:-15}
BIN=${BIN:-/home/ubuntu/netstack}; OUT=${OUT:-/tmp/netstack-bench}
SSH="sshpass -e ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10"  # -e: the password from SSHPASS, off the command line

lbl=$1; stack=$2; shift 2
[ "${1:-}" = "--" ] && shift
srv() { $SSH "$SERVER" "$@"; }
cli() { $SSH "$CLIENT" "$@"; }
stat_cmd='awk "/^cpu /{print \$2+\$3+\$4+\$6+\$7+\$8+\$9, \$8}" /proc/stat'
stop() { "$1" 'sudo pkill -INT -x netstack 2>/dev/null; for _ in $(seq 1 15); do pgrep -x netstack >/dev/null || break; sleep 1; done; sudo pkill -9 -x netstack 2>/dev/null; true'; }
setq() { srv "cur=\$(sudo ethtool -l $IFACE | tail -1 | awk '{print \$2}'); if [ $1 -lt \$cur ]; then sudo ethtool -X $IFACE equal $1; fi; sudo ethtool -L $IFACE combined $1; sudo ethtool -X $IFACE equal $1"; }

stop srv; stop cli

# Whatever happens from here on — a server that fails to start, a Ctrl-C —
# the box is put back: processes stopped, interrupts and queue count
# restored, the client's sysctls restored.
cleanup() {
  trap - EXIT INT TERM
  stop cli; stop srv
  cli 'echo "32768 60999" | sudo tee /proc/sys/net/ipv4/ip_local_port_range >/dev/null; echo 2 | sudo tee /proc/sys/net/ipv4/tcp_tw_reuse >/dev/null; echo 60 | sudo tee /proc/sys/net/ipv4/tcp_fin_timeout >/dev/null'
  srv "for f in $OUT/irq/*; do [ -f \$f ] || continue; i=\$(basename \$f); cat \$f | sudo tee /proc/irq/\$i/smp_affinity_list >/dev/null; done; rm -rf $OUT/irq"
  setq "$QUEUES"
}
# A trap that returns lets bash carry on with the script; on a signal it
# must exit after restoring, or the operator waits out the rest of the run.
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM
setq "$BUDGET"
srv "mkdir -p $OUT/irq; \
  for i in \$(ls /sys/class/net/$IFACE/device/msi_irqs); do n=\$(grep -E \"^ *\$i:\" /proc/interrupts | grep -oE 'mlx5_comp[0-9]+' | grep -oE '[0-9]+\$'); \
    [ -n \"\$n\" ] || continue; [ -f $OUT/irq/\$i ] || cat /proc/irq/\$i/smp_affinity_list > $OUT/irq/\$i; \
    if [ \"\$n\" -lt $BUDGET ]; then echo \$n | sudo tee /proc/irq/\$i/smp_affinity_list >/dev/null; fi; done"

# The server's GOMAXPROCS is not the budget: taskset enforces the cores, and
# netstack's pinned receive goroutines need Ps of their own beside the
# application's (see the README). The kernel's equivalent work is in softirq.
rot=""; for ((n=0; n<BUDGET; n++)); do rot="$rot,$(( (n+1) % BUDGET ))"; done
nsflags="-affinity=false -cpus ${rot#,} -gomaxprocs $((BUDGET*3)) -frames 32768 -txframes 8192 -ring 8192"
mode=files
case $stack in
  kernel)          sflags="-stack kernel -gomaxprocs $BUDGET $*" ;;
  kernel-sendfile) sflags="-stack kernel -gomaxprocs $BUDGET -sendfile $*" ;;
  netstack)        sflags="$nsflags $*" ;;
  netstack-zc)     mode=files-rtc; sflags="$nsflags -replicas ${REPLICAS:-8} $*" ;;
  *) echo "stack must be kernel, kernel-sendfile, netstack or netstack-zc"; exit 1 ;;
esac

# Many connections need many files and many ports on both sides.
srv "sudo prlimit --pid \$\$ --nofile=1048576:1048576 2>/dev/null; ulimit -n 1048576 2>/dev/null; true"
cli "ulimit -n 1048576 2>/dev/null; true"
tgt=$TARGET; [ "$PORTS" -gt 1 ] && tgt="${TARGET%:*}:$PORT-$((PORT+PORTS-1))"
srv "mkdir -p $OUT; sudo setsid ${SRVENV:+env $SRVENV} taskset -c 0-$((BUDGET-1)) $BIN -iface $IFACE -vlan $VLAN -ip $IP -port $PORT -ports $PORTS -mtu 1500 -zerocopy -mode $mode -mix '$MIX' $sflags > $OUT/$lbl.log 2>&1 < /dev/null & disown"
for _ in $(seq 1 40); do sleep 1; srv "grep -qE 'listening' $OUT/$lbl.log" && break; done
srv "grep -qE 'listening' $OUT/$lbl.log" || { echo "$lbl | SERVER FAILED: $(srv "tail -3 $OUT/$lbl.log" | tr '\n' ' ' | cut -c1-220)"; exit 1; }
# The server warns when the NIC strips VLAN tags before XDP sees them (a
# driver re-probe puts the offloads back on); a row measured like that is
# all zeros and worthless, so stop here rather than print one.
srv "grep -q 'WARNING' $OUT/$lbl.log" && { echo "$lbl | SERVER WARNED: $(srv "grep WARNING $OUT/$lbl.log" | cut -c21-240)"; exit 1; }
sleep 2

# The client can drive the workload from its own netstack (CLIENT_STACK),
# which costs it a fraction of what the kernel client costs: the churn rows
# otherwise measure the peer, not the server.
cflags=""
case ${CLIENT_STACK:-kernel} in
  kernel) ;;
  netstack) cflags="-client-stack netstack -zerocopy -iface $IFACE -vlan $VLAN -ip $CLIENT_IP -peer-mac $SERVER_MAC -affinity=false -gomaxprocs $((QUEUES*2)) -frames 32768 -txframes 8192 -ring 8192"; tgt=$TARGET ;;
  *) echo "CLIENT_STACK must be kernel or netstack"; exit 1 ;;
esac
cli "mkdir -p $OUT; rm -f $OUT/c-$lbl.log; (sudo setsid ${CLIENV:+env $CLIENV} prlimit --nofile=1048576:1048576 $BIN -client $tgt -files -workers $WORKERS -per-conn $PERCONN -mix '$MIX' ${SLOW:+-slow $SLOW} $cflags -duration ${DUR}s > $OUT/c-$lbl.log 2>&1 < /dev/null &)"
# Either client says it has started: the kernel one "files client:", the
# netstack one "via netstack on". Waiting for the wrong line measures an
# empty box after the run has finished.
for _ in $(seq 1 45); do sleep 1; cli "grep -qE 'files client:|via netstack on' $OUT/c-$lbl.log" && break; done
sleep 4
# the server process's own CPU too (utime+stime), so box noise is visible
proc_cmd='p=$(pgrep -x netstack | head -1); [ -n "$p" ] && awk "{print \$14+\$15}" /proc/$p/stat || echo 0'
# resident size of the server process, for the per-connection memory question
rss_cmd='p=$(pgrep -x netstack | head -1); [ -n "$p" ] && awk "/^VmRSS:/{print \$2}" /proc/$p/status || echo 0'
# Resident size is sampled by a loop on the server itself, not by a round
# trip a second from here: those would stretch the window they measure.
rssf=$(mktemp)
srv "m=0; for _ in \$(seq 1 $WIN); do p=\$(pgrep -x netstack | head -1); \
  r=\$(awk '/^VmRSS:/{print \$2}' /proc/\$p/status 2>/dev/null); \
  [ \"\${r:-0}\" -gt \"\$m\" ] && m=\$r; sleep 1; done; echo \$m" > "$rssf" 2>/dev/null &
rsspid=$!
# The window is what the clock says it was: the samples around it are ssh
# round trips, and dividing by WIN alone would misstate every CPU figure.
t0=$(date +%s.%N)
s0=$(srv "$stat_cmd"); p0=$(srv "$proc_cmd"); c0=$(cli "$stat_cmd")
sleep "$WIN"
s1=$(srv "$stat_cmd"); p1=$(srv "$proc_cmd"); c1=$(cli "$stat_cmd")
t1=$(date +%s.%N)
wait $rsspid 2>/dev/null; rssmax=$(cat "$rssf" 2>/dev/null); rm -f "$rssf"
WIN=$(echo "$t0 $t1" | awk '{printf "%.3f", $2-$1}')
for _ in $(seq 1 60); do sleep 1; cli "grep -q 'total over' $OUT/c-$lbl.log" && break; done
cores() { echo "$1" "$2" | awk -v w="$WIN" '{printf "%.2f (softirq %.2f)", ($3-$1)/100/w, ($4-$2)/100/w}'; }
pcores=$(awk -v a="$p0" -v b="$p1" -v w="$WIN" 'BEGIN{printf "%.2f", (b-a)/100/w}')
rssg=$(awk -v k="${rssmax:-0}" 'BEGIN{printf "%.2f", k/1048576}')
echo "$lbl [$stack, $BUDGET cores, $WORKERS workers, $PORTS ports${SLOW:+, slow $SLOW}, client ${CLIENT_STACK:-kernel}] server $(cores "$s0" "$s1") process $pcores rss ${rssg}G  client $(cores "$c0" "$c1")"
cli "grep -E 'total over|objects ' $OUT/c-$lbl.log | sed 's/^[0-9\/]* [0-9:]* //'" | sed 's/^/   /'
cleanup # the client tunes its box's ports and TIME-WAIT reuse; killed, it cannot restore them, so this does
