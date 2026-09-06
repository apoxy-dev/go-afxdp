# netstack: a userspace TCP server on AF_XDP

This example terminates TCP connections in userspace. It feeds
[gVisor's TCP/IP stack](https://pkg.go.dev/gvisor.dev/gvisor/pkg/tcpip)
(netstack) straight from the AF_XDP rings, so a real server — an echo service
here, and an HTTP handler behind the same code — runs entirely on packets the
kernel never sees. The point is to answer one question: once go-afxdp has taken
a packet off the wire at line rate, can a userspace TCP stack use that speed, or
does the handoff give it all back? The kernel stack is right there for
comparison; the same binary serves on it with `-stack kernel`.

The other examples (`fwd`, `natlb`, `dns`) rewrite headers and forward. This one
is the first that *speaks* TCP: it completes the handshake, tracks connection
state, reassembles streams, and retransmits — all above an `afxdp.Fleet`.

**To build, run, tune or embed it, read [`USAGE.md`](USAGE.md).** This file
is the results and how they were reached.

```
   client (kernel TCP)                     this box, one interface
 ┌────────────────────┐                ┌─────────────────────────────────┐
 │ nc / curl / wrk /  │  TCP/8080      │  XDP filter: tcp/8080 → AF_XDP   │
 │ the -client mode   │───────────────▶│  everything else → kernel (ARP,  │
 └────────────────────┘   VLAN 2131    │  SSH, …)                         │
                                        │        │                        │
                                        │   afxdp.Fleet (12 queues, ZC)   │
                                        │        │  xdplink.Endpoint       │
                                        │        ▼                        │
                                        │   gVisor netstack (IPv4 + TCP)  │
                                        │        │  gonet.ListenTCP        │
                                        │        ▼                        │
                                        │   echo / http handler (net.Conn)│
                                        └─────────────────────────────────┘
```

## Where the bytes go

gVisor owns every byte it touches: a `buffer.View` wraps a chunk gVisor
allocated from its own pool, and there is no way to hand it a slice you own.
So the datapath copies exactly once in each direction, and no more:

| step | cost |
|---|---|
| receive | one `copy` from the UMEM frame into a pooled view; the frame goes straight back to the fill ring |
| into the stack | `DeliverNetworkPacket` on the receive goroutine, synchronous down to the TCP demuxer |
| out of the stack | `WritePackets`, built directly into a UMEM frame with `SendFunc` — one `copy`, no staging buffer |

Everything else AF_XDP buys is kept: no per-packet syscalls, batched rings, no
`sk_buff`, and no kernel TCP on the path. True zero-copy into netstack is not
possible without forking `pkg/buffer`, and it turned out not to matter: nothing
below was limited by the copy. See the `xdplink` package doc for the details.

What *did* matter is how many times a byte's worth of headers goes through the
stack. Out of the box netstack sends and receives one MSS-sized segment at a
time, so 10 Gbit/s is 810k trips through TCP each way. The kernel does not do
that even with TSO off — and on this lab NIC TSO *is* off — it segments in
software once per 64 KiB. This example does the same thing in the endpoint:

- **Segmentation offload (`-gso`, default on).** The endpoint reports
  `HostGSOSupported`, so TCP hands it one packet of up to 64 KiB with the IPv4
  length covering all of it and only a pseudo-header checksum. `xdplink/gso.go`
  splits it into MSS-sized frames straight into the UMEM: headers copied per
  frame, IPv4 length and checksum redone, sequence number advanced, PSH and FIN
  kept for the last frame, TCP checksum finished per frame. One trip through the
  stack for 46 frames. Every packet from a connection carries that partial
  checksum, pure ACKs included, so the single-frame path finishes it too.
- **Receive offload (`-gro`, default on).** gVisor's GRO merges a burst of one
  flow's segments into a packet of up to 64 KiB before the stack sees it. It
  validates checksums itself before merging, so the endpoint claims RX checksum
  offload and verifies each frame first.
- **Checksum offload (`-txcsum`, default `auto`).** The stack's partial
  checksum is exactly what a NIC wants (Linux's `CHECKSUM_PARTIAL`), so with
  go-afxdp's `WithTxMetadata` the endpoint stops summing payload on transmit:
  each frame's metadata asks the NIC to finish the checksum from the TCP
  header, and the field gets the pseudo-header sum for that segment's length.
  That is a 16-byte stamp instead of a pass over every byte sent — at 10G it
  is ~1.25 GB/s of reads saved, at 100G ~12.5 GB/s. The catch: a driver that
  does not implement the request sends the frame untouched with a wrong
  checksum and no error anywhere. Forced on over ixgbe here, every segment
  arrived with a bad checksum (the client counted 56 `TcpInCsumErrors` and
  the connections died). So `auto` asks the kernel first
  (`afxdp.QueryXSKFeatures`, the netdev netlink family): ixgbe on 6.8 says no
  and gets software checksums; mlx5 says yes. The path is proven on veth,
  where the kernel computes the checksum from the same metadata in software
  (`TestVethTxChecksumOffload`); the offload numbers wait for the mlx5 box.

## Layer 2 without ARP

The endpoint answers no ARP and asks none. The XDP filter (`MatchTCPPort`) hands
the stack only its own TCP port, so ARP still reaches the kernel, and the kernel
keeps answering it for the service address — which it owns too, on the interface
or its VLAN sub-interface. Outbound frames still need a destination MAC, so the
endpoint learns the source MAC of every peer it hears from, keyed by IPv4 source
address. A server only ever replies to peers it has heard from, so the table
always has the answer.

## Transmit batching

gVisor calls `WritePackets` from many goroutines (application writers, the TCP
processors sending ACKs, retransmit timers), one packet per call by default. A
`Socket`'s transmit side wants one goroutine. The `fifo` queueing discipline
bridges the two: it runs one goroutine per queue, assigns each packet to a queue
by `pkt.Hash` (which TCP sets per connection), and batches up to 47 packets into
one `WritePackets` call. The endpoint selects the AF_XDP socket by the same
hash, so every socket's transmit side is driven by exactly one goroutine, no
lock, and gets batches. `-tx direct` removes the qdisc and serializes with a
per-socket mutex instead, to measure what the handoff costs.

## Flags, building, running, tuning

See [`USAGE.md`](USAGE.md): the quick start, the two builds (upstream and
fork), NIC preparation, every mode and flag, the load generator, the tuning
that matters, how to put `xdplink` under a stack of your own, and the
limitations. This file is the results and how they were reached.

## Results

Lab: two bare-metal boxes, Intel `ixgbe` 10G on VLAN 2131, native XDP zero-copy,
kernel 6.8, Go 1.26 toolchain, all 12 rx queues bound. The client is kernel TCP
on the peer box; the A/B is the same binary with `-stack kernel`. CPU is the
**server process's own** cores (`utime+stime`). For the kernel rows that
undercounts: the kernel's TCP work lands in softirq, not in the process, so
its cores column is the application only. The netstack column is everything.

Bulk, with segmentation and receive offload (`-gso -gro`, the defaults) and the
gVisor patches:

| workload | netstack | cores | kernel | cores |
|---|--:|--:|--:|--:|
| transmit, 1 flow (`-mode source`) | 9.18 Gbit/s | 0.47 | 9.36 | 0.70 |
| transmit, 4 flows | 9.36 Gbit/s | 0.55 | 9.39 | 0.14 |
| receive, 1 flow (`-mode sink`) | 9.39 Gbit/s | 1.16 | 9.39 | 0.29 |
| receive, 4 flows | 9.40 Gbit/s | 2.7 | 9.38 | 0.37 |
| bidirectional echo, 1 flow | 7.53 Gbit/s | 1.14 | 6.89 | 0.98 |
| bidirectional echo, 4 flows | 9.10 Gbit/s | 2.28 | 9.13 | 1.51 |

Line rate in every direction, with the whole stack in userspace, on one to
three cores. For the record, where this started: 3.7 Gbit/s receive, 0.83
transmit, and 0.13 bidirectional.

What the offloads themselves buy (`-gso=false -gro=false`, everything else the
same): single-flow transmit 6.8 instead of 9.1 and receive 6.3 instead of 9.4,
and at line rate with four flows about twice the CPU — transmit 2.6 cores
instead of 1.3, receive 4.3 instead of 2.4. The rest of the gain came from the
patches, need-wakeup, and buffer sizing, which help the per-segment path too.

Request/response, 64-byte messages. The netstack row is `-mode echo-rtc`
(run-to-completion, see below) with the fork's inline TCP processing
(`patches/0005`) and a 1 ms delayed ACK (`patches/0002`); the kernel row is the
goroutine-per-connection echo on kernel sockets:

| workload | netstack req/s | p50 | machine cores | kernel req/s | p50 | machine cores |
|---|--:|--:|--:|--:|--:|--:|
| 1 connection | 11.5k | 80µs | 0.14 | 11.0k | 90µs | 0.30 |
| 256 connections | **1,049k** | **210µs** | **8.2** | 908k–933k | 240µs | 11.2–11.5 |
| 1024 connections | **1,045k** | 880µs | 9.3 | — | | |

Four runs each, back to back, with the interface's NAPI sysctls at their
defaults for the kernel (go-afxdp sets them for itself and restores them on a
clean stop — a kernel row measured with them left behind reads 560k, which is
not the kernel's number). At 256 connections the userspace stack matches or
beats the kernel on requests per second, at lower latency, on three fewer
cores: 129k requests per core against 82k. Where this came from, each step
measured at 256 connections:

| variant | req/s | p50 | machine cores |
|---|--:|--:|--:|
| first version | 150k | 1.1ms | 1.5 |
| + need-wakeup, bigger pools | 458k | 520µs | 5.7 |
| + inline TCP processing (`patches/0005`) | 548k | 470µs | 6.5 |
| + direct transmit (`-tx direct`) | 661k | 340µs | 9.3 |
| + run-to-completion (`-mode echo-rtc`) | 720k | 320µs | 8.5 |
| + delayed ACK 1 ms (`patches/0002`) | 950k–1,010k | 220µs | 9.3 |
| + deferred kicks, transmit on the receive queue (`-deferkicks`, `-txhash-rx`) | 970k–990k | 210µs | 9.3 |
| + one stack per queue (`-stacks per-queue`, patch `0006`, now in `0014`) | 1,033k | 210µs | 9.0 |
| + integer clock arithmetic, no buffer zeroing (patches `0007` and `0008`, the latter now in `0016`) | **1,049k** | **210µs** | **8.2** |
| busy-poll, interrupt moderation off, GRO off, `GOMAXPROCS`, delayed ACK without rtc | no gain | | |

A profile of the loaded server after the second step said where the time
went: the largest item was raw syscalls — the `poll` that wakes a receive
loop and the `sendto` that kicks a transmit ring gone idle between replies —
and the rest of a request's round trip was goroutine handoffs: receive loop
to TCP processor to application to transmit goroutine, four wakeups where the
kernel pays one. The steps since remove three of them and half the kicks:

- **Inline TCP processing.** Upstream netstack always hands an inbound
  segment to a processor goroutine. The fork handles an uncontended
  established connection's segments on the receive goroutine, the way a
  kernel handles them in the softirq that received them — with care, because
  the delivery path holds the transport demuxer's read lock and closing an
  endpoint takes it for writing: anything that could close the connection
  still goes to the processor. (The first version did not, and deadlocked
  every receive loop after the first connection closed; `veth_fork_test.go`
  reproduces that in a second.)
- **Direct transmit.** The writer transmits itself instead of waking a
  per-queue goroutine.
- **Run-to-completion** (`rtc.go`). No goroutine per connection and no
  `net.Conn`: the server registers a waiter callback on the raw
  `tcpip.Endpoint` that only marks the connection ready (netstack fires it
  with the endpoint lock held), and each receive goroutine services its ready
  connections right after its batch — read, write back, next — on the same
  goroutine that received and processed the segment. This is the shape a
  proxy or load balancer wants anyway.
- **Delayed ACK.** With everything on one goroutine the reply is written
  microseconds after the segment is processed, so holding the bare ACK lets
  the reply carry it: frames and kicks per request go from two to one. On the
  goroutine echo the same hold cost more than it saved, because there the
  reply waited on a wakeup.

- **Deferred kicks.** The transmit ring is always parked between replies at
  these rates, so every reply was a `sendto`: about a million a second. Two
  things together turn that into one per batch. The fork sets a connection's
  transmit hash from the queue its segments arrive on, so every send for it —
  the ACK from inline TCP, the reply from the drain — goes out on the socket
  owned by the goroutine handling it. And go-afxdp gained `SendFuncNoKick`
  and `KickIfNeeded`, so the endpoint queues frames without the kick and each
  receive goroutine kicks its own socket once after its batch and drain.
  Kicks fell from 918k/s to 157–221k/s, level with the poll rate. A send from
  any other goroutine (a timer, the rare processor fallback) waits for that
  queue's next wake, at most a millisecond.

- **One stack per queue.** A single netstack has shared structures — the
  transport demuxer's read lock showed in every profile — and one processor
  pool. With `-stacks per-queue` (the default) every NIC queue gets its own
  stack: its own receive goroutine, transmit ring, demuxer, listener on the
  same port, and (with `patches/0006`) a single fallback processor. Nothing
  is shared but the XDP program and the address; RSS pins a connection to
  its queue and therefore its stack. Bulk is unchanged; request/response
  gains a few percent on fewer cores, and this is the shape 48 queues need.
- **A cheap clock and no buffer zeroing.** The profile at a million requests
  a second had 9% of CPU in time arithmetic: gVisor's `MonotonicTime.Add` and
  `Sub` go through `time.Unix(...).Add(d).Sub(...)` for what is an integer
  add, its clock reads wall and monotonic time for every reading, and TCP
  reads the clock several times per segment. patch `0008` (now in `0016`) makes the
  arithmetic integer arithmetic and the example supplies a `tcpip.Clock`
  that reads the runtime's `nanotime` once. `patches/0007` stops the buffer
  pool clearing every chunk it hands out (every user writes a fresh view
  before reading it; at 10G that clear was 1.25 GB/s of memset on receive).
  Together: 1,033k → 1,049k requests a second on 8.2 cores instead of 9.0,
  and transmit at line rate on 0.9 cores instead of 1.3.

What remains is the per-request work itself: at a million requests a second
on eight cores it is about 8µs of CPU per request across the receive copy, the
stack, the checksum both ways, and the reply. Of that, the syscalls are 12%,
gVisor's packet-buffer accessors (`headerView`, `PullUp`) about 10%, and its
pools 7%. Checksum offload through XDP metadata is built (`-txcsum`) and waits
for a NIC that honours it; the other levers are in the stack's per-packet
bookkeeping — and a second client box, because at these rates one peer may
be the ceiling.

### The load generator

A kernel client is the ceiling well before 100G: its TCP costs the peer box
what ours costs us, and at a million requests a second it is the bigger
share. `-client -client-stack netstack` runs the same stack on the client
box, the same way as the run-to-completion server: no goroutine per
connection, the receive goroutine that delivered a batch services every
connection it made ready. Same server (`-mode echo-rtc`), same 10 s window,
cores are whole-machine on each box:

| client | req/s | p50 | server cores | client cores |
|---|--:|--:|--:|--:|
| kernel, 256 connections | 1,061k | 210µs | 8.6 | 11.1 |
| netstack, 256 connections | **1,095k** | **190µs** | 8.7 | **9.0** |
| netstack, 1024 connections | 1,111k | 770µs | 9.4 | 9.4 |
| kernel, 4 flows sending 9.4 Gbit/s (`-bulk -sink`, TSO on) | | | 0.9 | 0.1 |
| netstack, 4 flows sending 9.38 Gbit/s | | | 1.2 | 0.4 |
| netstack, 4 flows receiving 9.36 Gbit/s (`-bulk -recv`) | | | 0.6 | 1.0 |
| netstack, 4 flows echoing 9.21 Gbit/s (`-bulk`) | | | 2.6 | 2.0 |

`bench/measure.sh` produces these rows: it waits for the client to report
it is running (a client that binds AF_XDP bounces its link for ~10 s at
startup) and samples both boxes over the steady state.

Making the stack a good *sender* against another netstack turned up two
things a server talking to kernel clients never showed:

- **A deep transmit queue burns a core per queue in the driver.** With an 8 MiB
  window on both sides the sender keeps thousands of frames queued; ixgbe's
  NAPI poll, finding its 512-entry hardware ring full, returns "busy" and is
  re-run at once — 8.5 million softirq iterations a second on each of three
  CPUs, doing nothing. The kernel avoids this for its own traffic with byte
  queue limits; AF_XDP leaves it to the application. `-txinflight` (default
  256) caps the frames a socket may have awaiting completion and holds the
  rest in the endpoint, in order, releasing them as completions come back
  on the receive goroutine's next batch. Nothing is dropped, and the
  client's cost for sending 9.4 Gbit/s went from 4.3 cores to 0.4.
- **Flushing GRO per batch makes the receiver ACK per batch.** A batch is a
  handful of frames at 10G, so the netstack receiver ACKed every 4 segments
  where the kernel (whose GRO holds across NAPI polls) ACKs every 27 — and
  every ACK is a wakeup, an inline send and a kick on the sender, which cost
  it 2.2 cores instead of 0.6. `-grohold` (default 200µs) lets a merge span
  batches until it is 64 KiB, ends in PSH or FIN, or has waited that long;
  request/response is untouched because PSH delivers at once.

Two smaller ones from the same work: a stop under load deadlocked (the stack
holds its lock while asking the endpoint to stop, and the endpoint's loops
were inside the stack waiting for it — the endpoint is stopped first now),
and the client box needed the same `ethtool -K eno2 rxvlan off` as the server
or the NIC strips the tag before XDP sees it; the example warns at startup.

### Transmit without copying: the content path

Everything above copies payload once on the way out: the stack's view into a
UMEM frame. For a server whose job is to hand out bytes it already holds — a
cache — that copy, and the checksum pass with it, are the whole per-byte
cost. `-mode source-zc` removes the copy:

- **The content lives in the UMEM.** With `-cache`, go-afxdp maps one UMEM
  for the whole fleet (`WithSharedMemory`) and registers that same region
  with every socket, so an address means the same thing on every queue.
  The content is laid out one segment per frame, with 160 bytes of room in
  front of each segment's payload for the headers of whichever connection
  sends it next (`xdplink/txcache.go`).
- **TCP never touches the bytes.** The fork's `WriteExternal` (`patches/0009`)
  queues a view over caller-owned memory without copying it, and the stack
  only counts, splits and clones such a view. One view covers 45 frames — the
  stack sees 63,900 contiguous bytes, the endpoint knows they sit in 45
  frames with gaps — so the stack does its per-segment work once per GSO
  packet, as it does for the copying path.
- **The endpoint writes headers, not payload.** For each frame the packet
  covers, it writes the link, IPv4 and TCP headers into the room before the
  payload, fixes length, sequence number and flags, finishes the checksum
  (or stamps the NIC's request), and puts a descriptor for the frame itself
  on the ring. A frame is on the wire for one connection at a time (its
  header room is shared); a busy frame, or a piece the stack split
  mid-frame, is copied into a pool frame instead, in order, so correctness
  never depends on the cache. The fleet reports the completion of a cache
  frame to the cache, which marks it free.

Serving a 177 MiB cache at line rate to four kernel receivers, server
process cores (the `source` row is the copying path, same binary, same run):

| | Gbit/s | process cores | of which |
|---|--:|--:|---|
| `-mode source` (copy) | 9.37 | 0.57 | memmove 27%, checksum 18% |
| `-mode source-zc`, one view per frame | 9.37 | 0.82 | view bookkeeping 35% |
| `-mode source-zc`, one view per 45 frames | 9.37 | **0.52** | checksum 35%, syscalls 22% |

99% of frames leave the cache in place. The first attempt was slower than
copying: forty-five separately allocated, cloned and released view objects
per packet cost more than moving 64 KiB does at 10G, which is worth knowing
about any zero-copy design. What remains is the software TCP checksum over
the payload, which is a NIC's job (`-txcsum`) on hardware that does it, and
the send syscalls; at 100G the copy this removes was 12.5 GB/s of memory
traffic.

### How it got here

Each step was a measured cause, not a guess, and each is reproducible with the
flags and patches in this directory:

1. **Reordering intolerance** (patch `0001`, now in `0015`). The link delivers ~0.2% of
   segments out of order to whichever end is receiving — the kernel sees the
   same — and netstack's RACK reorder window is `min_RTT/4`, a few
   microseconds here, so it called that loss 27 times more often than the
   kernel did. A floor on the window: transmit 0.83 → 3.21 Gbit/s.
2. **Per-segment cost.** Segmentation offload in the endpoint: transmit 3.21 →
   6.5 single-flow and 9.3 with four flows; request/response 150k → 490k/s
   with need-wakeup and bigger pools.
3. **GRO merged nothing** (patch `0003`, now in `0011`). Its checksum validation read from
   the wrong offset once a link header had been consumed, so it flushed every
   packet unmerged — silently, because fdbased marks checksums validated before
   GRO and never exercises the path. Fixed: receive 6.4 → 9.4 single-flow, and
   the merge ratio went from 1.0 to 5–7 frames per packet.
4. **Zero-window deadlock** (patch `0004`, now in `0012`). Under bidirectional bulk our
   receive window closed for a few milliseconds now and then; RFC 793 read
   literally then rejects the peer's retransmission at RCV.NXT *and its ACK*,
   so our sender never advanced, our application never drained, and the
   connection sat in RTO backoff. Linux processes the ACK of such a segment
   and drops its data. Doing the same: echo 0.46 → 4.5 Gbit/s; letting the
   sender see every ACK while the window is zero, plus a deeper echo queue so
   a short stall never closes the window: 4.5 → 9.0.

5. **No delayed ACK** (`patches/0002`). Halves the frames and kicks per
   request; pays off once the reply no longer waits on a wakeup (step 7).
6. **A wakeup per inbound segment** (`patches/0005`). Inline processing of an
   established connection's segments on the receive goroutine, plus direct
   transmit: request/response 458k → 661k/s.
7. **A goroutine per connection.** Run-to-completion on the raw endpoint,
   which also lets the delayed ACK ride the reply: 661k → ~1M/s, level with
   the kernel on fewer cores.
8. **A `sendto` per reply.** Transmit on the receive queue and kick once per
   batch: 1M/s at 1024 connections, and the lowest latency yet.
9. **One stack shared by all queues.** A stack per queue, shared-nothing:
   1,033k/s on nine cores, and the architecture a 48-queue NIC needs.

`-mode echo-simple` (read then write on one goroutine) remains the pathological
case for any stack whose write blocks; it is kept to measure exactly that.

## 100G: netstack against the kernel on mlx5

Two bare-metal boxes in São Paulo, ConnectX (mlx5_core, fw 22.40) at 100G
wired to each other, VLAN 2043, kernel 7.0, AMD EPYC 9275F (24 cores, 48
threads), one NUMA node. The device under test serves; the peer runs the
load generator — this binary, as a kernel client or as the netstack client
with one stack per queue. CPU is whole-machine busy time on the DUT over an
8 s window of the steady state (`bench/rig100.sh`); the wire carries 93.8
Gbit/s of TCP payload at most. The kernel runs its best case: TSO and GRO
on, 48 queues, the driver's own interrupt layout. netstack runs 16 queues,
`-frames 32768 -ring 8192`, everything else default.

**Unconstrained, kernel client on the peer**

| workload | netstack | cores | kernel | cores |
|---|--:|--:|--:|--:|
| transmit, 16 flows (`source`) | 93.5 Gbit/s | 4.3 | 93.7 | 0.8 |
| transmit, 1 flow | 22.8 Gbit/s | 0.8 | 35.7 | 0.2 |
| transmit from cache, no copy, 16 flows (`source-zc`) | 93.7 Gbit/s | **3.8** | — | |
| transmit from cache, 1 flow | **48.7 Gbit/s** | 0.6 | — | |
| receive, 16 flows (`sink-rtc`) | 90.2 Gbit/s | 6.1 | 93.8 | 3.8 |
| receive, 64 flows | 90.5 Gbit/s | 6.3 | 93.8 | 8.4 |
| echo, 16 flows | 44.9 Gbit/s | 8.6 | 93.1 | 5.5 |
| request/response, 1024 conns | 1.34M req/s | 9.0 | 1.29M | 15.2 |

The request/response row is the kernel client's ceiling (it is at 10.7
cores on the peer); with the netstack client on the peer the same DUT does
**1.88M req/s** at 1024 connections, p50 410µs, on 15.5 cores.

**Same core budget, netstack client on the peer** — the DUT confined to P
physical cores (its P queues, their interrupts and the process all on cores
0..P-1, the SMT siblings idle), the peer unconstrained. This is the
question: what does each stack do with a core.

| budget | netstack transmit (zc) | kernel transmit | netstack receive | kernel receive | netstack req/s | kernel req/s |
|--:|--:|--:|--:|--:|--:|--:|
| 4 cores | 89.7 Gbit/s (2.7) | 92.0 (1.2) | 39.8 Gbit/s (3.0)† | 84.4 (3.5) | **749k** | 595k |
| 8 cores | 91.1 (3.9) | 91.8 (1.3) | 58.5 (5.9)† | 93.2 (4.8) | **1.16M** | 947k |
| 16 cores | 91.5 (4.7) | 92.2 (1.4) | 90.9 (8.7) | 93.5 (4.5) | **1.69M** | 1.28M |

(cores actually busy in parentheses; 16 flows for bulk, 1024 connections
at 64 bytes for request/response, p50 400–1100µs for netstack, 720–1650µs
for the kernel. Receive rows are after the receive-path patches below.
† These two rows are bounded by the sender, not the DUT: it has a quarter
of its budget idle, no drops anywhere, and every flow sits at a congestion
window the peer's own stack stops growing (see "Receive, revisited").)

**Queues on the DUT**, request/response, 1024 connections, netstack client:

| queues | 8 | 16 | 24 | 32 | 48 |
|---|--:|--:|--:|--:|--:|
| req/s | 1.07M | 1.88M | 1.90M | 1.92M | 1.98M |
| DUT cores | 10.6 | 15.5 | 17.4 | 19.3 | 24.8 |

Sixteen queues is the knee; the last 32 queues buy 5% for 9 more cores.

### What the numbers say

- **Request/response: netstack wins at every budget, by 15–32%**, and at
  16 cores it is 1.69M against the kernel's 1.28M with the kernel's TCP work
  counted (softirq is in the machine-wide figure). The reasons are the 10G
  ones: no wakeup, no syscall and no copy per request, one stack per queue.
- **Transmit is a tie at the wire and the kernel does it for a third of the
  CPU, because of TSO.** The NIC segments 64 KiB for the kernel; AF_XDP has
  no segmentation offload, so netstack writes headers and a descriptor per
  frame — 8.2M times a second. Zero copy took the payload copy out (4.3 →
  3.8 cores at 16 flows, and one connection went from 22.8 to 48.7 Gbit/s);
  the per-frame work is what is left, and it is not a copy.
- **Receive is where the kernel is better: 93 Gbit/s on 4.5 cores against
  90 on 8.7 after the fixes below (75 on 12.3 before).** What remains in the
  profile is the copy of every frame into a stack buffer (21%, the cold
  read of DMA'd memory), the poll and kick syscalls (10%), checksum
  validation (5%), and TCP. The kernel's GRO merges page fragments in place
  and copies nothing until the application reads. Views over the UMEM
  frames themselves (the fork's external views make that possible now)
  would take the copy's write half and the chunk allocation, perhaps
  another 5–10%; the cold read of the frame stays with whoever touches it
  first.
- **Echo (bidirectional bulk) at 45 Gbit/s** is the same receive cost plus
  the transmit cost on the same core per flow, and is the weakest row.

### Receive, revisited

The first receive profile blamed GRO for cloning a view per frame. It was
not GRO: after GRO a packet is ~35 views, and gVisor cloned that whole view
list three times on the way to the application — when TCP created the
segment (`pkt.Clone()`), when it trimmed the headers, and when the
application read (`SubApply` clones and releases every view). About 30% of
receive CPU was that churn. Two small fork patches removed the two that
matter (`patches/0010`, `0011`); the trim runs on one-view frames after GRO
and was left alone:

| receive, 16 flows, `sink-rtc` | before | after |
|---|--:|--:|
| 16 queues, unconstrained | 91.3 Gbit/s on 9.0 cores | 92.1 on **6.6** |
| 16-core budget | 75.0 on 12.3 | 90.9 on 8.7 |
| 8-core budget | 48.0 on 6.5 | 58.5 on 5.9 |
| 4-core budget | 33.4 on 3.3 | 39.8 on 3.0 |

The budget rows then needed two more things to be fair. First, placement:
confined with `taskset`, a queue's receive goroutine could land on the core
of its own interrupt, which starves it; the script now pins each worker one
core over from its interrupt (`-cpus`). Second, the peer. At the 8-core
budget the DUT has a quarter of its cores idle, no drops at the NIC, in the
fleet, or at any endpoint, and still moves 58 Gbit/s — because every flow
is sitting at a congestion window of about 330 segments with a 1 ms round
trip, and the sender's stack does not grow it. With the kernel as the
sender the story is different and worse: intermittently, in most runs, the
DUT's receiver counts thousands of segments arriving out of order (a
counter added for this, `tcp.OutOfOrderSegments`) and the kernel's own
`TCPSpuriousRtxHostQueues` admits it reordered its stream across its
transmit queues; tens of thousands of retransmits follow and its windows
collapse. The netstack client never does that: zero out-of-order arrivals,
zero retransmits, every run. So the confined receive rows measure the
sender as much as the DUT, and a load generator that can drive a confined
receiver to the wire is the missing instrument.

### A CDN-shaped workload: many clients, mixed object sizes

The rows above are a handful of long-lived flows. A content server's traffic
is the opposite: thousands of clients dialling, fetching objects of very
different sizes, and hanging up. `-mode files` / `-mode files-rtc` serve
that and `-client -files` drives it (`bench/files.sh`). The protocol is as
dumb as a server can be — the client sends `GET /<name>` and the server
writes that object's bytes — so the measurement is the stack, not a parser.
The client closes first, so TIME-WAIT lands on the client, where
`tcp_tw_reuse` recycles it.

The server is handicapped to **4 physical cores** (4 NIC queues, their
interrupts on those cores, the process pinned to them, SMT siblings idle)
and the client, on 48 threads, uses whatever it needs. Mix: 8 KiB, 32 KiB,
128 KiB and 1 MiB objects, weighted 50/30/15/5, 2000 client workers, 30 s.
Both stacks serve without copying: the kernel with `sendfile(2)` from tmpfs,
netstack from the UMEM cache (`files-rtc`, below). Cores are whole-machine
busy time on the server box during a 20 s window in the middle of the run;
it wanders by up to half a core between runs on the churn row.

| server, 4 cores | connections/s | objects/s | Gbit/s | cores | p50 first byte |
|---|--:|--:|--:|--:|--:|
| **one object per connection** (pure churn) | | | | | |
| kernel + sendfile | 32.6k | 32.6k | 22.4 | **1.2** | 17.2 ms |
| netstack (`files-rtc`) | 31.4k | 31.4k | 21.6 | 2.4 | 18.2 ms |
| **four objects per connection** (the mix) | | | | | |
| kernel + sendfile | **30.5k** | **122k** | **83.7** | **2.6** | 10.2 ms |
| netstack (`files-rtc`) | 26.2k | 105k | 71.9 | 2.9 | **0.85 ms** |
| **100 objects per connection** (keep-alive) | | | | | |
| kernel + sendfile | 1.4k | **136k** | **93.5** (the wire) | **2.6** | **1.5 ms** |
| netstack (`files-rtc`) | 1.3k | 123k | 84.4 | 2.9 | 3.1 ms |

#### The client was the ceiling

Every row above is driven by the kernel client, and on the churn row that
client is what is being measured: it spends about 1.1 ms of CPU per
connection against the server's 40 µs, so at 33k connections a second it is
flat out on 48 threads while the four-core server is half idle.
`-client-stack netstack` drives the same workload from the
run-to-completion path instead (`client_files.go`), and costs 125 µs per
connection — nine times less. With that client the churn rows measure the
server, and the picture changes:

| driven by the netstack client | connections/s | objects/s | Gbit/s | server cores |
|---|--:|--:|--:|--:|
| churn, 4 cores, kernel + sendfile | **77.9k** | 77.8k | **53.3** | 2.8 |
| churn, 4 cores, netstack | 61.7k | 61.7k | 42.3 | **2.4** |
| churn, 8 cores, kernel + sendfile | 91.5k | 91.4k | 62.8 | **4.2** |
| churn, 8 cores, netstack | **109.6k** | 109.5k | **75.3** | 4.5 |
| mix, 8 cores, kernel + sendfile | 34.0k | **136k** | **93.2** (the wire) | **5.1** |
| mix, 8 cores, netstack | 33.2k | 133k | 91.3 | 5.3 |

At eight cores the two are within 2% on the mix — both near the wire, for
the same CPU — and netstack opens 20% more connections a second than the
kernel does. At four cores the kernel is still 26% ahead on pure churn.
Per connection: 36 µs for the kernel, 39 µs for netstack.

The churn profile has no hot spot; the cost of a connection is many small
things. The handshake is 9% (endpoint construction, the SYN-ACK), the FIN
4%, timers 6%, the collector 6%. Patch `0016` trims what could be trimmed
without redesign: gVisor's registry of closing endpoints, kept for the
sentry's save and taken under one global mutex twice per close by every
stack in the process, is off; a delayed-ACK timer that a reply has carried
is stopped rather than left to fire and find nothing to do, which it did
once per request; and the SACK scoreboard's tree — the largest allocation
an endpoint made — is built on the first SACK block, which a churning
connection never sees. Together 2.5% more connections for 3% less CPU. The
rest is what a connection costs this stack. (The server's `tcp:` line
shows about 1.8k timeouts on these rows, all in the final second: the
client drops its 2000 open connections at its deadline and the server's
in-flight sends time out. During the run there are none.)

This client also found the last bug of the campaign, and it was not
gVisor's. The netstack server's resident size under churn read 6.8 GiB
against the kernel's 0.14, and a GC ceiling only traded it for connection
rate — so it looked like allocation rate. An allocation profile said the
whole run had allocated 6 GB, about 3.5 KiB per connection, which a
collector clears without noticing; the memory was *held*. The
run-to-completion server kept every connection it had ever accepted on a
list, for the error counters that live on an endpoint rather than the
stack, and never took a closed one off. Now a closing connection folds its
counters into a running total and leaves the list in O(1): 0.35 GiB at 60k
connections a second, and 4% more of them, since the collector no longer
walks two million dead endpoints. The other allocator worth removing was in
the transmit path — the closure that builds frames captured the segmenter
and escaped on every packet run — and it is now a struct the queue reuses.
What gVisor itself allocates per connection is about 6 KiB (endpoint,
sender with its SACK scoreboard, timers), 360 MB/s at this rate, not worth
pooling.

The churn rows here are the kernel client's ceiling (it is at 30 to 34 of
48 threads' worth of busy time, i.e. flat out, and its acknowledgements
leave 8 ms after the data arrives); the section below drives them with a
client that is not the limit. On established
connections it is ahead by 12 to 17%: it reaches the wire on 100-object
connections where netstack stops at 82 Gbit/s, and on the mix it serves
17% more connections and objects for 10% less CPU. Netstack's first byte
arrives in 0.85 ms against the kernel's 10 ms on the mix — the
run-to-completion server has no queue of goroutines in front of a request —
but on keep-alive the order reverses, 3.1 ms against 1.5: a request on a
connection that has already had its turns joins the back of the transmit
line (below), and with the box saturated waits about half a round of it.
The first-byte p99 is 80–95 ms against the kernel's 25–40 on every row; on
the wire the server's own p99 is 10 ms (a capture of 5000 connections on
the 100 MB row), so the rest is four saturated cores, on both stacks.

The client staggers its workers' first dials over a second (`-ramp`).
Started in the same instant, 2000 workers hand the server one burst of
2000 requests, and that burst alone was the whole run's first-byte p99 —
130 ms on netstack, 36 on the kernel — for a server that, once running,
answers in 10. Clients of a real server arrive spread out.

**A caution on the CPU column.** It is whole-machine busy time from
`/proc/stat`, which is sampled at the timer tick: a process that wakes for
short bursts is charged less than it used it. On the saturated rows above
it agrees with the server process's own accounting to two decimal places;
on a row that leaves the box mostly idle (the slow-client row below) it
reads about half of what the process itself reports, and the process
figure is the one to believe.

Where this started, same bench, same budget: netstack's first file server
(goroutine per connection, `Write` from a buffer) did 17.0k connections/s,
68k objects/s and 46.5 Gbit/s on the mix, and 10.9k before a `GOMAXPROCS`
mistake was found (below). Each step was a profile first:

| step | connections/s | Gbit/s | what the profile said |
|---|--:|--:|---|
| goroutine per connection, `Write` from a buffer | 17.0k | 46.5 | 17% memmove: every object copied twice (into the stack, into frames); 2000 writers contending for the endpoint and transmit locks |
| `files-rtc`: run-to-completion, objects in the UMEM cache, `WriteExternal` | 21.7k | 59.4 | copies gone; 30% now the processor goroutine handling SYN and the handshake ACK, its SYN-ACK sends contending with the receive goroutine |
| `tcp.BatchProcessing`: that work on the receive goroutine after the batch | 25.0k | 68.7 | 7% delayed-ACK timers firing per request for nothing; 10% IPv4 header checksum recomputed per frame |
| timer disarmed when a reply carries the ACK; incremental IP checksum; 32 replicas of each object | 25.9k | 71.1 | what is left: TCP itself, gVisor's packet-buffer accessors (6%), clock reads (4%), the handshake (5%) |

**One thing to get right when benchmarking this.** The first netstack runs
looked far worse (10.9k connections a second) because the server was given
`GOMAXPROCS=4` to match its 4-core budget. Its four receive goroutines are
locked to their threads, so they occupied all four Ps and the application
goroutines never ran. The core budget is enforced by `taskset`; `GOMAXPROCS`
must still be large enough for the stack's own goroutines *plus* the
application — 8 to 12 is the knee here, and beyond it nothing changes. The
kernel does not have this problem because its equivalent work runs in softirq
threads outside the Go runtime, which is precisely why the comparison has to
count the machine's cores rather than the process's.

#### Tens of thousands of connections, some of them slow

A content server's connections are not 2000 busy ones: they are tens of
thousands, most of them idle or slow. `-ports N` gives the server that many
consecutive ports (one four-tuple space each, since the client dials from
one address), the client holds a connection open for the whole run
(`-per-conn 0`), and `-slow` paces each one so it behaves like a viewer on
a thin link. Resident size is the server process's peak.

| open connections, 4 cores | Gbit/s | cores | server memory |
|---|--:|--:|--:|
| 2000, kernel + sendfile | **93.5** | 2.8 | **0.03 GiB** |
| 2000, netstack | 75.6 | 3.1 | 0.34 GiB |
| 20000, kernel + sendfile | **92.4** | 3.1 | **0.15 GiB** |
| 20000, netstack | 71.5 | 3.1 | 0.59 GiB |
| 50000, kernel + sendfile | **93.5** | 2.9 | **0.31 GiB** |
| 50000, netstack | 69.6 | 3.1 | 0.96 GiB |
| 20000 slow (50 KB/s each) | 8.15 (all the demand) | ns 1.2, kernel 0.3 | ns 0.64, kernel 0.13 |

The kernel is flat: 50000 connections cost it what 2000 do. Netstack now
nearly is — it gives up 8% between 2000 and 50000 — but only after two
fixes that this workload found, both of them cases of a stack mistaking a
slow reader for a lost packet. Before them the 50000-connection row ran at
40 Gbit/s with 433k retransmit timeouts, and the slow-client row cost 2.7
cores instead of 1.2.

- **A run-to-completion pass needs a budget** (`-rtc-budget`, 200µs). The
  receive goroutine serviced *every* ready connection before returning to
  the ring. With 12500 connections on a queue that pass took longer than a
  retransmit timeout, so acknowledgements sat unread in the ring while the
  sender timed out data the peer already had. A NAPI poll has a budget for
  the same reason. It is time, not a count of connections: a count that
  suits 50000 slow connections (four) puts a short request behind a queue
  of long ones when there are only 2000. The endpoint helps by telling the
  loop when the application left work behind, so a bounded pass continues
  on the next turn of the loop instead of waiting for a packet.
- **A zero-window probe is not a loss** (fork, `-zwp-separate`). gVisor
  sends it from the retransmit timer and then treats it exactly like a
  retransmit timeout: it counts one, collapses the congestion window and
  resets the SACK scoreboard. So every time a slow reader stopped reading,
  its connection restarted from a minimal window. Linux keeps window
  probes on their own timer. Separating them halved the slow row's CPU
  (2.7 cores to 1.2) and took its timeouts from 840k to zero.
- **A window-limited sender has no tail to probe** (fork,
  `-tlp-skip-window-limited`). The tail loss probe was arming while the
  sender still had data ready that only the peer's window held back. The
  peer has those bytes and has not read them, so the probe can only
  retransmit data that is not lost, and it fired on every object a slow
  reader fetched: 2.6M probes in a 30 s run, cut to 1.4M by this and to
  180k with the two together.

What remains is memory and the last of the CPU: netstack holds about 20 KiB
per open connection against the kernel's 6 KiB, and 20000 slow connections
cost it four times the kernel's CPU for the same 8 Gbit/s. Neither is a
cliff, and 50000 connections in under a gibibyte is a number a CDN can plan
around.

#### Overload: backpressure instead of drops

With 100 MB objects in the mix (64 KiB, 1, 5 and 100 MB, weighted
50/30/15/5, the same 2000 workers) the first version of this server fell
over. The packets a queue could not yet put on the ring waited in a backlog
behind the socket; with 2000 senders that backlog grew until a packet's
round trip through it passed the retransmit timeout, and beyond its cap the
endpoint dropped. gVisor then counted every refused or dropped packet as a
loss: RTO storms, tens of thousands of spurious retransmits, dial timeouts,
and after the first few seconds almost nothing delivered. The kernel does
not do this because TCP small queues bound what one socket may have below
TCP and wake the socket when its packets complete, and `fq` shares the
device among flows.

The equivalent here is in three parts, patch `0014` and `xdplink`:

- **The link refuses instead of queueing.** Above a high-water mark
  (`-txbacklog`, 128 packets, about 8 MB or 3 ms of one queue's wire) the
  endpoint's `WritePackets` takes nothing and returns `WouldBlock`. Control
  packets — ACKs, SYN-ACKs, window probes, anything under 128 bytes that
  is not a FIN or RST — are never refused and go ahead of the backlog: a
  refused SYN-ACK is a second of handshake timeout at the peer, and the
  first version lost thousands of them.
- **The sender treats that as "not sent".** Upstream advances SND.NXT
  whether or not the link took the segment. With `tcp.LinkBackpressure`
  the attempt is undone (send time, count, the delayed-ACK it thought it
  carried, no retransmit timer), the endpoint is marked blocked and joins
  a per-stack line, and it stops sending until told.
- **Turns, in order.** After every receive batch that leaves the backlog
  below low water the endpoint calls `tcp.ResumeBlocked`, which gives the
  waiting connections, in the order they were refused, one segment each
  (`-link-turn`; a segment is a GSO packet of up to 64 KiB, already twenty
  times fq's quantum, and two per turn cost keep-alive 2 ms of first byte)
  and stops at the first the link refuses again, so nobody loses their
  place. A connection that never had a turn goes to the front,
  the way `fq` lets a new flow's first packets through. Without the turns
  the first connections resumed refilled the backlog with a whole window
  each and the rest waited through hundreds of cycles: 81k timeouts and
  5k spurious RTOs in one run, a different collapse.

`TestVethBackpressure` drives a bulk transfer through a two-packet backlog
and checks byte-exact delivery with refusals and no retransmits. On the rig,
same 4 cores, 30 s, 2000 workers:

| 100 MB mix, 4 cores | connections/s | objects/s | Gbit/s | cores | first byte p50 | errors | server timeouts |
|---|--:|--:|--:|--:|--:|--:|--:|
| kernel + sendfile | 595 | 2.2k | **93.6** (the wire) | **2.4** | **2.0 ms** (p99 21) | 0 | 59 |
| netstack (`files-rtc`), backpressure | 604 | 2.2k | 92.1 | 3.2 | 3.4 ms (p99 46) | 0 | **0** |
| netstack, `-link-backpressure=false` (the old behaviour) | — | — | — | — | — | client never finished | 22.8k (+4.4k SYNs dropped) |

Netstack is within 2% of the wire on this row with zero timeouts and zero
spurious RTOs. Its first byte was 16 ms when this section was first
written; two things brought it to 3.4. The run-to-completion server's ready
list now has two lanes, and a connection with something to *read* — a
request, or the peer's close — goes ahead of the hundreds that merely have
room to send more: with the pass bounded (`-rtc-budget`), a request behind
every transfer in progress waited most of those 16 ms. And the backlog's
marks are 64/32 packets rather than 128/64: half the queue in front of a
new connection's first segment, for slightly more throughput as well. What
is left against the kernel's 2 ms is four saturated cores. Two things the run showed about the peer, not the server: the
client's default 1024-entry receive ring overflowed at 80 Gbit/s across
2000 flows (`rx_out_of_buffer` +64k per run), and every one of those drops
was a retransmit charged to the server until the ring was deepened (item 8
below). And the `dsack` count is the peer telling the server its tail loss
probes were unnecessary: gVisor's probe timeout is twice the smoothed RTT
with no floor, which with a precise sub-millisecond RTT is a few hundred
microseconds, less than the peer's delayed acknowledgement, so it probed
almost every tail. Patch `0015` floors it at 10 ms as Linux does
(`-min-pto`) and stops it arming for a window-limited sender at all; with
the zero-window probe off the loss path too (`0014`), the 50000-connection
row went from 1.27M probes and 433k timeouts to 4.3k and 36. What remains
are objects whose last acknowledgement a saturated client sends 20 ms late
(the churn row), one 1420-byte probe each, 0.7% of the bytes.

### What the mlx5 path needed

- **The NIC's checksum offload works through XDP transmit metadata** — the
  first hardware to honour it here (`tx csum nic 99M sw 0`) — but with mlx5's
  `xdp_tx_mpwqe` on, about one frame in ten thousand went out with the
  pseudo-header partial still in the checksum field (the peer counted
  `TcpInCsumErrors`, 0 with the flag off). The driver's multi-packet work
  queue entry packs several frames into one entry and loses the per-frame
  request. go-afxdp now turns the flag off whenever transmit metadata is on.
- **Draining on the receive goroutine matters more than anything.** A
  goroutine-per-connection sink allocates every view on the receive
  goroutine and frees it on another, which turns every pooled view into a
  cross-P steal in `sync.Pool`: 30% of the profile, 48 Gbit/s on 10.6 cores.
  `-mode sink-rtc` reads on the receive goroutine: 89 Gbit/s on 5.8 cores.
  Trusting the NIC's receive checksums instead of verifying them in
  software (`-rxcsum trust`) changed nothing — the checksum was never the
  cost. gVisor's leak-check registry (a locked map insert per packet) is
  now off.
- **Busy polling hurt** on this driver and kernel (84 vs 89 Gbit/s, more
  CPU); the 128k pps/queue cap it was meant for did not appear. Deeper
  rings (`-frames 32768 -ring 8192`) cut retransmits four-fold at the same
  rate.
- **A client needs one stack per queue too.** One stack over 16 queues was
  slower than the kernel client (859k req/s). A client chooses its source
  port before RSS decides the queue, so `rss.go` reads the NIC's Toeplitz
  key and indirection table (`ethtool -x`) and picks, for each stack, ports
  whose replies hash to its queue. With that the netstack client drives
  1.88M req/s and both bulk directions at the wire, on fewer cores than
  the kernel client.
- Two bugs of mine found only at this rate: the zero-copy planner shared
  one scratch list across the packets of a batch (frames of other packets
  were sent again: reordering, retransmits, 66 Gbit/s), and the client's
  totals were divided by wall time including a slow close after the
  deadline (they are snapshotted at the deadline now).

## Running it on a 100G mlx5 box

The commands the section above was run with, in order, are in
[`USAGE.md`](USAGE.md#running-it-on-a-100g-mlx5-box).

## Limitations

Moved to [`USAGE.md`](USAGE.md#limitations).
