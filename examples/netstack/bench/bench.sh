#!/bin/bash
# bench.sh — one measurement matrix for examples/netstack, run from the server box.
#
#   bench/bench.sh <label> [rows] -- [server flags...]
#
# rows: comma-separated subset of rr,source,sink,echo (default: all four).
# Everything after "--" is passed to the server (e.g. -stack kernel, -gso=false).
#
# For each row it starts the server, drives the built-in client on the peer
# box, and prints one line: throughput or req/s, the server process's own CPU
# (utime+stime, so unrelated load on the box is excluded), the server's TCP
# retransmit counters, and the client kernel's reorder/retransmit view from
# nstat. Bulk rows run one at a time and never right after a connection-churn
# row (which floods the client with TIME_WAIT sockets).
#
# Assumes: root SSH to the client, the binary at /root/netstack on the client,
# the lab addressing in the variables below. Adjust and keep it in the README.
set -u
IFACE=${IFACE:-eno2}; VLAN=${VLAN:-2131}; IP=${IP:-192.168.0.1/24}; PORT=${PORT:-8080}
CLIENT=${CLIENT:-root@206.223.228.217}; TARGET=${TARGET:-192.168.0.1:8080}
DUR=${DUR:-8}; LINKWAIT=${LINKWAIT:-26}
BIN=${BIN:-$(dirname "$0")/../netstack}
OUT=${OUT:-/tmp/netstack-bench}; mkdir -p "$OUT"

label=$1; shift
rows="rr,source,sink,echo"
if [ "${1:-}" != "--" ] && [ -n "${1:-}" ]; then rows=$1; shift; fi
[ "${1:-}" = "--" ] && shift
server_flags=("$@")

cpu_ticks() { awk '{print $14+$15}' "/proc/$1/stat" 2>/dev/null || echo 0; }
HZ=$(getconf CLK_TCK)
NCPU=$(nproc)
# machine-wide busy time from /proc/stat: everything except idle+iowait, in ticks
sys_busy() { awk '/^cpu /{print $2+$3+$4+$6+$7+$8+$9}' /proc/stat; }

# Stop gracefully first: go-afxdp restores the NAPI sysctls and IRQ steering
# it changed only on a clean Close, and a kernel row measured with those left
# behind is not the kernel's own number.
stop_server() { pkill -INT -x netstack 2>/dev/null; for _ in $(seq 1 10); do pgrep -x netstack >/dev/null || break; sleep 1; done; pkill -9 -x netstack 2>/dev/null; sleep 1; }

start_server() { # $1 = mode
  stop_server
  setsid "$BIN" -iface "$IFACE" -vlan "$VLAN" -ip "$IP" -port "$PORT" -mode "$1" -mtu 1500 -zerocopy \
    "${server_flags[@]}" > "$OUT/$label-$1.log" 2>&1 < /dev/null &
  disown
  for _ in $(seq 1 "$LINKWAIT"); do sleep 1; grep -q "listening" "$OUT/$label-$1.log" && break; done
  grep -q "listening" "$OUT/$label-$1.log" || { echo "$label | $1 | SERVER FAILED (see $OUT/$label-$1.log)"; return 1; }
  sleep 2
  pgrep -x netstack | head -1
}


# run_client <pid> <name> <client args...>: one client run with CPU and nstat capture
run_client() {
  local pid=$1 name=$2; shift 2
  local c0 c1 out cores tot lat nst b0 b1 mcores
  c0=$(cpu_ticks "$pid"); b0=$(sys_busy)
  out=$(ssh -o BatchMode=yes "$CLIENT" "nstat -n >/dev/null; /root/netstack -client $TARGET -duration ${DUR}s $* 2>&1 | grep -E 'total over|latency'; nstat 2>/dev/null | grep -E 'TCPOFOQueue|TCPDSACKOldSent|TcpRetransSegs|TcpInCsumErrors' | awk '{printf \"%s=%s \", \$1, \$2}'")
  c1=$(cpu_ticks "$pid"); b1=$(sys_busy)
  cores=$(awk -v a="$c0" -v b="$c1" -v hz="$HZ" -v d="$DUR" 'BEGIN{printf "%.2f", (b-a)/hz/d}')
  # the client run includes ~1s of setup; the machine-wide figure is over the same wall time
  mcores=$(awk -v a="$b0" -v b="$b1" -v hz="$HZ" -v d="$DUR" 'BEGIN{printf "%.2f", (b-a)/hz/d}')
  tot=$(echo "$out" | grep "total over" | sed 's/.*total over [0-9.]*s: //; s/ (.*//')
  lat=$(echo "$out" | grep "latency" | sed 's/.*latency //')
  nst=$(echo "$out" | grep -oE "Tcp[A-Za-z]+=[0-9]+" | sed 's/TcpExt//; s/Tcp//' | tr '\n' ' ')
  echo "$label | $name | $tot | ${lat:-} | proc-cores $cores machine-cores $mcores | client: $nst"
}

server_tcp() { grep "tcp: invalid" "$OUT/$label-$1.log" | tail -1 | grep -oE "fastRetx [0-9]+ slowRetx [0-9]+ dsack [0-9]+"; }
server_offload() { grep "offload:" "$OUT/$label-$1.log" | tail -1 | sed 's/.*offload: //'; }

for row in ${rows//,/ }; do
  case $row in
    rr)
      pid=$(start_server echo) || continue
      run_client "$pid" "rr-c1-m64"   -conns 1   -msg 64
      run_client "$pid" "rr-c256-m64" -conns 256 -msg 64
      stop_server ;;
    source)
      pid=$(start_server source) || continue
      run_client "$pid" "source-c1" -conns 1 -bulk -recv
      run_client "$pid" "source-c4" -conns 4 -bulk -recv
      echo "$label | source | server: $(server_tcp source) | $(server_offload source)"
      stop_server ;;
    sink)
      pid=$(start_server sink) || continue
      run_client "$pid" "sink-c1" -conns 1 -bulk -sink
      run_client "$pid" "sink-c4" -conns 4 -bulk -sink
      echo "$label | sink | server: $(server_tcp sink) | $(server_offload sink)"
      stop_server ;;
    echo)
      pid=$(start_server echo) || continue
      run_client "$pid" "echo-c1" -conns 1 -bulk
      run_client "$pid" "echo-c4" -conns 4 -bulk
      echo "$label | echo | server: $(server_tcp echo) | $(server_offload echo)"
      stop_server ;;
    *) echo "unknown row $row" ;;
  esac
done
