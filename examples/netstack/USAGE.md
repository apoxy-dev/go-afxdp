# Using the netstack example

This is the how-to. `README.md` is the story and the numbers; this file is
what you need to build it, run it, drive load at it, tune it, and put the
link endpoint under a stack of your own.

## What you get

- **`xdplink`**, a `stack.LinkEndpoint` for gVisor's netstack over an
  `afxdp.Fleet`: one receive goroutine per NIC queue, transmit built straight
  into UMEM frames, host-side segmentation (GSO), receive merging (GRO),
  NIC checksum offload through XDP transmit metadata where the driver has
  it, 802.1Q, and backpressure into the stack when a queue is full. That is
  the reusable part.
- **`netstack`**, a server and load generator around it: echo, HTTP, bulk
  source and sink, and a file server that serves objects out of a
  zero-copy content cache, each with a run-to-completion variant that
  services connections on the receive goroutine with no goroutine per
  connection. The same binary serves on the kernel with `-stack kernel`,
  which is the A/B for every number in the README.
- **A gVisor fork**, as patches in `patches/`: fixes and knobs the numbers
  depend on. Everything builds and runs against upstream gVisor without
  them; with them it is a different stack under load. `patches/README.md`
  says what each one does and why.

IPv4 and TCP only. The kernel keeps the address, ARP and everything that is
not TCP to the served ports.

## Quick start

```
cd examples/netstack && go build .
sudo ./netstack -iface eno2 -vlan 2131 -ip 192.168.0.1/24 -port 8080 -mode echo
```

From another host: `nc 192.168.0.1 8080`, `curl` with `-mode http`, or the
built-in client:

```
./netstack -client 192.168.0.1:8080 -conns 256 -msg 64 -duration 10s   # request/response
./netstack -client 192.168.0.1:8080 -conns 8 -bulk -duration 10s       # throughput
./netstack -client 192.168.0.1:8080 -files -workers 2000 -per-conn 4 \
    -mix 8k:50,32k:30,128k:15,1m:5 -duration 30s                       # a CDN-shaped load
```

The A/B, same box, same flags: add `-stack kernel`.

gVisor's module needs a newer Go toolchain than the core library; `go`
fetches it when `GOTOOLCHAIN` is `auto` (the default). The example is its
own module so the core library keeps its small dependency set.

## Before the first run

- **Root**, or `CAP_NET_ADMIN` and `CAP_NET_RAW`, and a memlock limit that
  fits the UMEMs (the program raises it; 16384 × 2 KiB frames per queue by
  default, 32768 with the settings below).
- **The address belongs to the kernel.** The stack answers no ARP; the
  kernel does, because it owns the address on the interface or its VLAN
  sub-interface. Configure it there first.
- **MTU no larger than one frame receives.** A 2048-byte frame receives
  1792 bytes (`Socket.MaxPacket`: the frame less the kernel's 256-byte
  headroom); mlx5 silently refuses a zero-copy socket for an interface
  whose MTU is larger, and go-afxdp falls back to copy mode, which costs a
  copy per frame in softirq and halves the connection rate. The server
  prints a `WARNING` naming the MTU and the limit when that happens, and
  `bench/files.sh` refuses to measure a server that warned. Keep eno2 at
  1500, or use `-frames` with larger frames.
- **VLAN offload off**, or XDP never sees the tag and every frame is
  dropped as the wrong VLAN (the program warns at startup):
  ```
  ethtool -K eno2 rxvlan off txvlan off rx-vlan-filter off
  ```
- **The filter takes a whole port.** Any TCP to `-port` (and the `-ports`
  after it) on the box goes to the stack, from every address. Narrow it
  with `pcapfilter` for real use.
- **Queues.** By default every receive queue is bound and gets a stack of
  its own. Past 16 queues the NIC is the limit and the CPU keeps climbing;
  set the count on the NIC, not with `-queues`, so RSS spreads over exactly
  the bound queues:
  ```
  ethtool -X eno2 equal 16 && ethtool -L eno2 combined 16
  ```
  (Shrinking: `-X` before `-L`; growing: after. mlx5 refuses otherwise.)

## The two builds

**Upstream.** `go build .` uses the gVisor version in `go.mod`. It works, and
bulk transfers on a real link are poor: the stack mistakes ordinary
reordering for loss, GRO merges nothing, and a closed window can wedge a
bidirectional connection. Those were the first three patches (`0001`,
`0003`, `0004`, now folded into later ones; `patches/README.md`).

**Fork** (every number in the README):

1. Check out gVisor's `go` branch: `git clone -b go https://github.com/google/gvisor ../../gvisor-fork`.
2. Apply the patches, all of them, in order, from the checkout's root:
   `for p in path/to/patches/*.patch; do patch -p1 < "$p"; done`. Every
   file the fork changes belongs to exactly one patch, so this applies
   cleanly with no rejects; the table in `patches/README.md` says what each
   one does, including the six whose change now lives inside a later one.
3. Point a `go.work` at it (gitignored here; the shape is
   `use .` and `use ../../gvisor-fork`).
4. `go build -tags gvisorfork .` — the tag turns on the fork-only flags
   (`-delayed-ack`, `-inline-tcp`, `-batch-tcp`, `-link-backpressure`,
   `-min-pto` and the rest) and the run-to-completion modes that need
   fork APIs (`files-rtc`, `source-zc`).

Tests: `go test -tags gvisorfork .` runs the veth suite as root (a veth pair,
a namespace, generic XDP), including byte-exact checks of the zero-copy
paths, backpressure and the netstack file client. `GOWORK=off go test .`
checks the upstream build. Do not `go vet` inside the fork checkout; it
vets all of gVisor.

## Modes

| `-mode` | what it does | notes |
|---|---|---|
| `echo` | writes back what it reads, a goroutine per connection over `net.Conn` | the kernel-comparable default |
| `echo-rtc` | the same, run to completion on the receive goroutines | netstack only; the request/response number |
| `http` | a fixed 200 for `wrk`/`ab`/`curl` | |
| `sink`, `sink-rtc` | read and discard | one-way receive |
| `source`, `source-zc` | stream a pattern | one-way transmit; `source-zc` streams the `-cache` in place, fork only |
| `files` | serve objects of the sizes in `-mix` to `GET /<name>\n` | on the kernel, `-sendfile` serves them from tmpfs with `sendfile(2)` |
| `files-rtc` | the same out of the UMEM cache, transmitted in place, `-replicas` copies of hot objects | fork only; the CDN number |

## The load generator

`-client ip:port` turns the binary into the client. `-client-stack kernel`
(default) uses plain sockets; `-client-stack netstack` runs the same stack,
run to completion, one stack per queue with source ports chosen from the
NIC's RSS table (`ethtool -x`; without a readable table it falls back to one
stack over all queues). It needs `-iface`, `-vlan`, `-ip` for its own side
and `-peer-mac` for the server's MAC, so nothing of the kernel's is on the
path.

The kernel client costs about 1.1 ms of CPU per connection and is the
ceiling near 33k connections a second; the netstack client costs 125 µs and
is what the churn rows use. Both take `-files -workers N -per-conn M -mix …`
(`-per-conn 0` keeps connections open for the run), `-slow <bytes/s>` to
pace each connection like a slow viewer, and `-ramp` to spread the first
dials (default 1 s; a synchronized start is a burst no real server sees).
The kernel client also takes a target port range `ip:8080-8087` to match a
server's `-ports 8`; the netstack client dials one port. The netstack
client's source ports come from a cycle per queue, and when the cycle comes
round faster than the client stack's 200 ms TIME-WAIT clears (a ramped
start on an idle 100G server does this), a connection waits a TIME-WAIT
out and dials again rather than failing.

## Tuning that matters

- **Cores.** The core budget is enforced with `taskset`, and `-gomaxprocs`
  must be *larger* than it — three times is the knee. The receive
  goroutines are locked to their threads; give them exactly the budget in
  Ps and the application never runs. The kernel does its equivalent work
  in softirq outside the process, which is also why a fair comparison
  counts the machine's cores, not the process's.
- **Confining the process yourself.** `-affinity=false -cpus <list>` pins
  queue *i*'s receive goroutine to the *i*-th CPU in the list; put each one
  core over from its own interrupt (`bench/files.sh` does), or a receiver
  shares a core with its IRQ and starves.
- **Rings.** `-frames 32768 -txframes 8192 -ring 8192` on both sides at
  100G; the defaults cut retransmits four times worse. `-txinflight`
  (256) bounds frames on the transmit ring per queue; keep it under the
  NIC's hardware ring or the driver spins a core per queue.
- **Backpressure.** `-txbacklog high,low` (64,32 packets) bounds a queue's
  transmit backlog; above it the stack is refused and pauses, and the
  waiting connections take turns of one segment (`-link-turn`). `0` is the
  old behaviour, drops, which collapses into retransmit storms with large
  objects and thousands of connections. `-tx direct` only; under the fifo
  qdisc a refusal never reaches the sender.
- **The pass budget.** `-rtc-budget` (200 µs) bounds how long a
  run-to-completion pass services connections before going back to the
  ring. Without it 50000 connections take three times the timeouts and
  half the throughput; with it a keep-alive request's first byte costs a
  millisecond or two more.
- **Buffers.** `-sndbuf`/`-rcvbuf` default to 8 MiB each, sized for one
  10G stream; the stack allocates lazily, so 50000 open connections still
  fit in a gibibyte, but 1 MiB is the safer default for many connections.
- **Ports.** `-ports N` serves N consecutive ports: one four-tuple space
  each, for a client that holds tens of thousands of connections from one
  address. Raise the file limit on both boxes.
- **Reading the 1 Hz report.** `conns` is the application's view (accepted,
  active, bytes each way); `xdp rx/tx` the rings; `tcp seg … est … retx`
  the stack; the `tcp:` line is the stack's loss counters — `timeouts` are
  RTOs, `dsack` is the peer saying a retransmit was unnecessary, `tlp` tail
  loss probes, `failedAttempts` SYNs it could not take; the `ep:` line is
  the link — `txDeferred` waited for ring room, `txRefused` was pushed back
  into the sender, `txDropped` should be zero, `wrongVLAN`/`unknownMAC` mean
  a setup problem; `mem:` is resident size, heap and goroutines. A netstack
  client also prints `sender:` (cwnd and RTT of the first connections) and
  `syscalls:` (polls and kicks over the run).

## Putting xdplink under your own stack

The example's `startNetstack` in `main.go` is the reference; the shape is:

```go
fleet, _ := afxdp.Open(iface, afxdp.WithFilter(afxdp.MatchTCPPort(port)),
    afxdp.WithKeepManagement(), afxdp.WithTxMetadata(), afxdp.WithNeedWakeup())
ep, _ := xdplink.NewForSockets(fleet.Sockets()[q:q+1], xdplink.Config{
    MAC: mac, VLAN: vlan, MTU: 1500, GSO: true, GRO: true,
    TxChecksumOffload: features.TxChecksum, TxInflight: 256,
    TxBacklogHigh: 64, TxBacklogLow: 32,       // with the fork's tcp.LinkBackpressure
    OnTxRoom: func() { tcp.ResumeBlocked(s) }, // fork
    AfterBatch: func() bool { tcp.DrainDeferred(s); return app.drain() }, // fork + run-to-completion
})
s := stack.New(stack.Options{NetworkProtocols: {ipv4.NewProtocol},
    TransportProtocols: {tcp.NewProtocolCUBIC}, Clock: clk})
s.CreateNICWithOptions(1, ep, stack.NICOptions{})   // TxDirect: no qdisc; or QDisc: fifo.New(ep, ep.NumQueues(), 1000)
ep.AddNeighbour(peerIP, peerMAC)                    // if the stack must send first
```

Contracts to respect:

- One `Endpoint` per queue (`NewForSockets` with one socket) gives each
  stack its own receive goroutine and transmit ring, shared-nothing. `New`
  over the whole fleet is one stack over all queues; then `WritePackets`
  picks the socket by `pkt.Hash` and the fifo qdisc gives each socket one
  writer.
- `Capabilities` claims RX checksum offload because GRO needs it; the
  endpoint verifies every frame first (`-rxcsum trust` skips that).
- `AfterBatch` runs on each receive goroutine after every batch and on every
  poll timeout, with no lock held, and returns whether it left work behind;
  while it does, the loop polls without blocking.
- `OnTxRoom` runs on the receive goroutine after a flush, no lock held,
  whenever the backlog is below high water; make it cheap when nothing
  waits.
- The tuple `TxBacklogHigh > 0` and a stack that treats `WouldBlock` as
  "not sent" is what makes backpressure safe; on upstream gVisor leave the
  marks at 0 or every refusal is a loss.
- Shutdown: close listeners, `ep.Close()`, then `Stack.Destroy()`, then
  `fleet.Close()`. Destroy holds the stack's lock while it waits for the
  receive loops, so they must be stopped first.
- `WithTxMetadata` forces mlx5's multi-packet WQEs off; with them on the
  NIC loses about one checksum request in ten thousand.

## How it works, in one page

**Where the bytes go.** gVisor owns every byte it touches; there is no way
to hand its buffers a slice you own. So a received frame is copied once
into a pooled view and goes straight back to the fill ring, and on
transmit the packet is copied once into a UMEM frame. Content served from
the cache (`files-rtc`, `source-zc`) is the exception: the cache lives in
the UMEM, the stack is handed views over it, and the frames go out in
place with only headers written. What costs more than the copy is how
often a header's worth of bytes goes through TCP: the endpoint reports
host GSO, so TCP hands it one packet of up to 64 KiB and it segments in
place, and GRO merges a burst of one flow into one packet on the way in.

**Checksums.** The stack leaves a partial (pseudo-header) checksum, exactly
what a NIC wants. With transmit metadata the endpoint stamps a request per
frame and the NIC finishes it (mlx5); without, it finishes in software
(ixgbe on 6.8 ignores the request silently, so `-txcsum auto` asks the
driver first).

**Layer 2.** No ARP in the stack. The endpoint learns the source MAC of
every peer it hears from; a server only replies to peers it has heard from,
and a client is told the server's MAC with `-peer-mac`.

**Transmit.** `-tx direct` (default): the writing goroutine transmits
itself, under a per-queue mutex, with the kick deferred to the receive
goroutine's next pass. `-tx fifo`: gVisor's fifo qdisc, one goroutine per
queue, batches of up to 47 packets — fewer syscalls, more latency, no
backpressure.

**Run to completion.** The rtc modes register a waiter callback that only
marks a connection ready; the receive goroutine that delivered the batch
services the ready connections right after it, on the same goroutine, in
two lanes (connections with something to read first) for at most
`-rtc-budget` before returning to the ring.

## Running it on a 100G mlx5 box

The commands, in order, that the README's 100G section was run with:


1. **Queues: 8–16, not 48.** One stack per queue costs a receive goroutine, a
   32 MiB UMEM and a softirq share each, and past 16 queues the NIC is the
   limit while the CPU keeps climbing (see the root README's per-queue
   table). Set it on the NIC, not with `-queues`, so RSS spreads over exactly
   the queues that are bound:
   ```
   ethtool -X eno2 equal 16 && ethtool -L eno2 combined 16
   ```
2. **Check the startup lines.** `xsk features: zero-copy true, ... tx-checksum
   true` followed by `transmit checksums: NIC offload` means the metadata path
   is live; on a driver that reports `tx-checksum false` it falls back to
   software and says so. `Fleet.Info()` is printed too: expect
   `napi defer=2 flush=20µs` and one worker CPU per queue.
3. **First three runs**, in this order, and look at the `syscalls:` line of
   each 1 Hz report (polls, kicks, suppressed) next to the throughput:
   ```
   ./netstack -iface eno2 -vlan 2131 -ip <ip/24> -mode sink              # receive: is GRO merging (frames/pkt > 3)?
   ./netstack -iface eno2 -vlan 2131 -ip <ip/24> -mode sink -busypoll 50  # same, with kernel busy polling
   ./netstack -iface eno2 -vlan 2131 -ip <ip/24> -mode echo-rtc          # request/response, 256+ connections
   ```
   On this rig busy polling was a loss (see above); try it, keep it only if
   `xdp rx` per queue sits at ~128k/s without it.
4. **Two NUMA nodes.** go-afxdp registers each queue's UMEM from a thread
   bound to that queue's worker CPU, so the frames and the DMA land on the
   NIC's node when the workers do. Check `Info()`'s CPU list against
   `cat /sys/class/net/eno2/device/numa_node`.
5. **Buffers.** `-sndbuf`/`-rcvbuf` default to 8 MiB, sized for one 10G
   stream; at 100G with many connections 1 MiB each is the safer default.
6. **`-txinflight` against the mlx5 send queue.** The default 256 is set
   for ixgbe's 512-entry ring. mlx5's XSK send queue is larger (check
   `ethtool -g`); if transmit is short of line rate with `txDeferred`
   climbing in the `ep:` line, raise it — but keep it under the queue size,
   or its NAPI poll busy-waits the same way.
7. **Load.** Two 10G kernel clients cannot fill it. `-client-stack netstack`
   on a peer with its own AF_XDP NIC drives request/response at a higher
   rate on fewer cores than the kernel client, and bulk at line rate on
   about one core each way.
8. **Many connections need ports and files.** `-ports 8` on the server and
   `ip:8080-8087` on the client: one four-tuple space per port, since the
   client dials from one address. Raise the file limit on both boxes
   (`prlimit --nofile=1048576`); `bench/files.sh` does.
9. **The kernel client's receive ring.** mlx5's default is 1024 entries;
   at 80+ Gbit/s spread over 2000 flows it overflows (`ethtool -S eno2 |
   grep rx_out_of_buffer` climbs by tens of thousands per run) and every
   drop is a retransmit the server gets blamed for. `ethtool -G eno2 rx
   8192` on the client before the file-fetch rows; `bench/files.sh` does
   not do it for you.

## Benchmarks

- `bench/bench.sh <label> [rows] -- [server flags]`: the 10G matrix (rr,
  source, sink, echo), both stacks, one line per row with the server
  process's CPU and the client's reorder/retransmit view.
- `bench/measure.sh <label> <mode> -- <client flags>`: one row with
  whole-machine CPU sampled on both boxes in the client's steady state.
- `bench/rig100.sh`: the 100G rows, with `BUDGET=N` confining the server
  to N cores (queues, interrupts, `taskset`, pinning).
- `bench/files.sh <label> <kernel|kernel-sendfile|netstack|netstack-zc> -- [server flags]`:
  the file workload. `BUDGET`, `WORKERS`, `PERCONN`, `MIX`, `PORTS`,
  `SLOW`, `CLIENT_STACK=netstack`, `SRVENV="GOGC=…"`, `DUR`/`WIN`. It
  prints throughput, latency per size class, and the server's whole-machine
  and process CPU and peak resident size over a measured window. Whole-
  machine CPU from `/proc/stat` is tick-sampled and undercounts a bursty
  process; on the saturated rows it agrees with the process figure, on the
  slow-client row believe the process figure.

## Limitations

- **IPv4 and TCP only.** No IPv6, UDP or ICMP in the stack; ICMP still comes
  from the kernel, which owns the address.
- **No ARP in the stack.** Move the address fully into netstack and you must
  add `arp.NewProtocol` and answer ARP yourself. Replies go to the MAC each
  peer's frames came from, keyed by peer IP, in a table bounded at 4096
  entries with eviction: right for a lab segment or a subnet, but on a routed
  network every client address is an entry and the right answer is a
  gateway MAC option, which this does not have yet.
- **Backpressure has one resume path.** A connection the link refused sends
  again only when the link calls `OnTxRoom` (or on its next ACK); if a link
  never did, it would wait for good. The endpoint here calls it after every
  batch with room, and the veth test covers it, but a new link must too.
- **The filter is a whole port** (`-ports` of them), not a VIP.
- **Receive copies every frame once**, and the kernel is ahead on pure
  receive for it (92 Gbit/s on 6.6 cores against the kernel's 3.8 at 16
  queues).
- **No TSO.** AF_XDP has nothing like it, so the kernel transmits bulk on a
  third of the CPU. Segmentation here is per frame in software; only the
  checksum is offloaded.
- **Slow readers cost 3 to 4× the kernel's CPU** per connection, and an
  open connection about 20 KiB against the kernel's 6.
- **The content cache is a demonstration.** Synthetic pattern, one 1420-byte
  segment per 2 KiB frame, `-replicas` copies of each object because a
  frame busy on the wire is not shared. A real cache would size frames to
  the MTU and load files.
- **The netstack client needs a readable RSS table** (`ethtool -x`) to give
  each stack its own queue; without one it falls back to a single stack,
  slower than the kernel client past a dozen queues.
- **Proof of concept**, not a server to run: no TLS, no HTTP beyond a fixed
  reply, no configuration beyond flags.

## Flags

| flag | meaning |
|---|---|
| `-iface` | interface to bind (default `eth0`) |
| `-vlan` | 802.1Q tag on every frame (0 = untagged) |
| `-ip` | service address as `ip/prefix` (default: the interface's, or its VLAN sub-interface's, first IPv4) |
| `-port` | TCP port to serve (default 8080) |
| `-mtu` | IP MTU the stack assumes (default 1500) |
| `-queues` | rx queues to bind (0 = all; RSS spreads connections across them) |
| `-stack` | `netstack` (gVisor over AF_XDP) or `kernel` (plain sockets, the A/B) |
| `-mode` | `echo` or `http` (a fixed 200 for `wrk`/`ab`) |
| `-tx` | netstack transmit path: `direct` (default: the writing goroutine transmits itself, lowest latency) or `fifo` (a goroutine per queue batches transmits; fewer syscalls under bulk) |
| `-gso` | segmentation offload: TCP hands the endpoint 64 KiB packets and it splits them (default on) |
| `-gro` | merge received bursts before the stack (default on) |
| `-sndbuf`, `-rcvbuf` | netstack TCP buffers per connection, bytes (default 8 MiB each: a 10G bulk stream needs it; 1 MiB fits 100k connections in 100 GiB) |
| `-grohold` | let a GRO merge span receive batches for up to this long (default 200µs; 0 = flush every batch). PSH and FIN still deliver at once, so request/response is unaffected; a bulk receiver ACKs per merged packet instead of per batch |
| `-txinflight` | frames a queue may have in the transmit ring awaiting completion (default 256; 0 = the ring's depth). The rest wait in the endpoint and go out on completions, in order. Keep it under the NIC's hardware ring or the driver spins a core per queue waiting for room — see the README's Results |
| `-cache` | bytes of content held in one UMEM shared by every socket, for `-mode source-zc` (default 0: none) |
| `-ports` | serve this many consecutive ports from `-port`: one four-tuple space per port, so a single client address can hold tens of thousands of connections |
| `-rtc-budget` | how long one run-to-completion pass services ready connections before it goes back to receiving (default 200µs; 0 = until the ready list is empty). `-rtc-budget-conns` bounds the same pass by connection count |
| `-slow` | with `-client -files`: pace each connection to this many bytes a second, so connections live for seconds and the server holds their data. Both clients: the kernel one sleeps between reads, the netstack one pauses a connection on a timer and gives it a 64 KiB receive buffer |
| `-ramp` | with `-client -files`: spread the workers' first dials over this long (default 1s), so the run does not open with one burst of `-workers` requests. Both clients |
| `-txbacklog` | `high,low` (or just `high`, then low is half) water marks, in packets, of a queue's backlog of packets waiting for ring room (default `64,32`; fork build, `-tx direct`). At high the stack's write is refused and the connection pauses; below low it is given a turn. ACKs, SYN-ACKs and window probes are never refused and go ahead of the backlog. `0` is the old behaviour: a backlog capped by drops. Fork flags beside it: `-link-backpressure` (off = refusals count as losses, the control), `-link-turn` (segments per turn, default 1), `-min-pto` (floor of the tail loss probe timeout, default 10 ms), `-tlp-skip-window-limited` (no probe while only the peer's window holds the sender back), `-zwp-separate` (a zero-window probe is not a retransmit timeout) |
| `-rxcsum` | `verify` (default) or `trust` the NIC's receive checksums instead of re-validating in software; a measurement knob (it changed nothing at 100G) |
| `-txcsum` | TCP checksum on transmit: `auto` (default: NIC offload through XDP transmit metadata when the driver reports it, else software), `on`, `off` |
| `-needwakeup` | bind with `XDP_USE_NEED_WAKEUP` so a zero-copy driver needs no syscall per transmit batch (default on) |
| `-frames`, `-txframes`, `-ring` | UMEM frames per queue (16384), of which reserved for transmit (8192), and ring depth (4096); a GSO batch can need 2200 transmit frames |
| `-stacks` | `per-queue` (default: one independent netstack per NIC queue, shared-nothing) or `single` (one stack over all queues) |
| `-deferkicks` | queue transmits without a kick; each receive goroutine kicks its socket once per batch (default on) |
| `-busypoll` | kernel busy-poll microseconds for the sockets (0 = off) |
| `-gomaxprocs` | cap the Go scheduler (upstream gVisor also starts that many TCP processor goroutines; the fork starts `-tcp-processors`, default 1) |
| `-mode files` | serve objects of the sizes in `-mix` (`GET /<name>` → that many bytes), goroutine per connection; on the kernel, `-sendfile` serves them from tmpfs with sendfile(2) |
| `-mode files-rtc` | the same on the run-to-completion path with the objects in the UMEM cache, transmitted in place (fork build); `-replicas` copies of each object so hot ones do not contend for frames |
| `-mix` | the object mix as `size:weight` pairs, e.g. `8k:50,32k:30,128k:15,1m:5` |
| `-files`, `-workers`, `-per-conn` | with `-client`: run the file-fetch load generator with this many goroutines and this many objects per connection |
| `-cpus` | pin queue i's receive goroutine to the i-th CPU in this list (with `-affinity=false`), for an operator who places threads themselves |
| `-mode` (also) | `sink-rtc` (netstack only: drain on the receive goroutines — the 100G receive mode), `echo-rtc` (netstack only: run-to-completion on the receive goroutines, the fastest), `echo-simple` (one goroutine, read then write — the worst case for a stack with a blocking write), `sink` (drain, one-way receive test), `source` (stream, one-way transmit test), `source-zc` (netstack with the fork: stream the `-cache` content without copying it) |
| `-recovery` | netstack TCPRecovery bitmask; `0` disables RACK (for the reorder investigation) |
| `-cpuprofile`, `-memprofile`, `-mutexprofile`, `-blockprofile`, `-trace`, `-trace-secs` | write a CPU, allocation, mutex-contention or blocking profile, or an execution trace, at stop |
| `-autotune` | let go-afxdp tune NAPI defer/flush sysctls on the interface (restored on a clean stop) |
| fork build only: `-reorder-window`, `-reorder-nodupthresh`, `-delayed-ack`, `-inline-tcp`, `-txhash-rx`, `-tcp-processors`, `-chunk-clear`, `-batch-tcp`, `-precise-rtt`, `-seg-clone`, `-link-backpressure`, `-link-turn`, `-min-pto`, `-tlp-skip-window-limited`, `-zwp-separate` | the fork's knobs, one per patch; `patches/README.md` says what each restores or changes. Defaults are the measured best; they exist so a reader can turn any one off and see what it was worth |
| `-client` | run as a load generator against `ip:port` instead of serving (`-conns`, `-msg`, `-duration`, `-bulk`, `-sink`, `-recv`, `-reconnect`) |
| `-client-stack` | with `-client`: `kernel` (default, plain sockets) or `netstack` — the same stack, driven run-to-completion, as the load generator, one stack per queue with source ports chosen by the NIC's RSS; needs `-iface`/`-vlan` (and `-ip` unless the VLAN sub-interface has one). It drives `-files` too, at about a ninth of the kernel client's CPU per connection |
| `-peer-mac` | with `-client-stack netstack`: the server's MAC, so the kernel's ARP table is not consulted |
| `-affinity` | let go-afxdp place workers and steer interrupts (default on); off when the operator confines the process (`bench/rig100.sh` `BUDGET=`) |
