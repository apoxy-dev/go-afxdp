# go-afxdp

A small, easy to use Go library for **AF_XDP** sockets. It moves packets between
a NIC and userspace at high rates, bypassing the kernel network stack, for
DPDK-like speeds with the convenience of ordinary Go.

```go
import "github.com/atoonk/go-afxdp"
```

It binds every rx queue for you, installs an in-kernel filter so you only take
the traffic you want, auto-selects zero copy where the driver supports it, and is
safe to drive from a receive and a transmit goroutine at once. It is a
friendlier, concurrency-safe fork of [`asavie/xdp`](https://github.com/asavie/xdp).

New to AF_XDP? It is a different beast from `net.UDPConn`. Read
[How AF_XDP works](#how-af_xdp-works-and-why-theres-a-filter) for the one-minute
mental model (especially *why there is a filter*), then come back.

**Performance:** about 13 Mpps transmitting 64-byte frames from
userspace Go on one Intel ixgbe NIC, roughly 92% of 10G line rate.

**Status:** validated on Intel ixgbe (zero copy) and AWS ENA. The API is still
settling, so expect minor changes before a v1.0 tag.

## Install

```
go get github.com/atoonk/go-afxdp
```

Linux, Go 1.22+, and `CAP_NET_RAW` (or root) with enough locked memory
(`ulimit -l`) for the BPF maps and UMEM.

## Quick start

### Receive

Pick the traffic you want with a filter, open the interface, read packets. `Open`
attaches the XDP program, binds one socket per rx queue, and registers them, all
in one call.

```go
fleet, err := afxdp.Open("eth0", afxdp.WithUDPPorts(4789)) // capture UDP/4789
if err != nil {
    log.Fatal(err)
}
defer fleet.Close()

for _, xsk := range fleet.Sockets() {
    go func(xsk *afxdp.Socket) {
        for {
            xsk.Fill(xsk.NumFreeFillSlots()) // give the kernel buffers
            n, _ := xsk.Poll(-1)             // block until packets arrive
            descs := xsk.Receive(n)
            for _, d := range descs {
                frame := xsk.GetFrame(d)     // the received bytes
                _ = frame
            }
            xsk.Recycle(descs)               // return frames to be filled again
        }
    }(xsk)
}
```

The whole receive model is **Fill, Poll, Receive, Recycle**. Only UDP/4789
reaches your sockets; everything else (SSH included) keeps flowing through the
kernel normally.

### Transmit

Hand `SendBatch` (or `SendFunc`) your packets and it does the ring bookkeeping for
you, reclaiming sent frames, kicking the kernel, never stalling on a full ring.
Just call it in a loop. A loop that sends many small packets per batch can use
`SendFuncNoKick` and one `KickIfNeeded` at the end of the batch: one syscall
per batch instead of one per packet. With `WithoutTxKick`, `Transmit` only puts
the frames on the ring, and a second goroutine calls `KickIfNeeded`; in copy
mode the kernel copies the frames in that kick, on the second core.

```go
fleet, _ := afxdp.Open("eth0", afxdp.WithFilter(afxdp.MatchNone())) // transmit only
xsk := fleet.Sockets()[0]
for {
    n, err := xsk.SendBatch(packets) // returns how many were queued this call
    ...
}
```

A filter is required, `Open` returns an error without one, so you cannot
accidentally redirect every packet and cut off your own box. Pass
`WithUDPPorts`/`WithFilter` for specific traffic, `MatchAll()` to take everything
on purpose, or `MatchNone()` for transmit only.

## How AF_XDP works (and why there's a filter)

If you have only used `net.UDPConn` and friends, AF_XDP works differently enough
to be worth a paragraph before you start.

A normal socket (the `AF_INET` family) hands you data *after* the kernel's network
stack has processed the packet. **AF_XDP** is its own socket family that receives
raw Ethernet frames straight from the **driver**, before the stack, and that is
where the speed comes from.

But the driver has to be told *which* frames go to your socket instead of up the
normal stack. That decision is an **XDP program**, a small eBPF program that runs
in the driver on every received packet and returns either `XDP_PASS` (let the
kernel handle it normally) or `XDP_REDIRECT` (hand it to an AF_XDP socket). So
receiving with AF_XDP is always two pieces working together, the socket, and an
eBPF/XDP filter that redirects the traffic you want into it.

Writing, compiling, and loading that eBPF is the part most libraries leave to you.
**This library installs it for you.** `WithUDPPorts(53)` (or the more general
`WithFilter(...)`) compiles to the XDP program, attaches it to the interface, and
points its redirect at your sockets. Everything that does not match keeps flowing
up the normal kernel stack untouched. Transmit is the mirror image, you write
frames into shared memory (the UMEM) and the driver sends them.

The takeaway: a filter is not an optional extra. For receive it is *how packets
reach an AF_XDP socket at all*, so choosing it is the main thing you configure.
(`MatchNone` covers the transmit-only case, where you want the datapath but no
redirect.)

**Seeing what's installed.** `Fleet.Info()` reports the active filter and mode
(`... filter udp/53`). From a shell, `ip link show <iface>` shows whether an XDP
program is attached, and `bpftool net show dev <iface>` lists it. If expected
traffic is not arriving, check that `Info().Filter` matches it and that `Stats()`
is not reporting `rx_ring_full`/`fill_empty`, which mean the rings could not keep
up.

## When to use AF_XDP

AF_XDP is for when you need packets **in userspace**. If all you do is reflect,
forward, drop, or mirror packets, do it in the XDP program itself (`XDP_TX`,
`bpf_redirect()`, `XDP_DROP`), it stays in the driver and is faster than a
userspace round trip. Reach for AF_XDP when the per-packet logic does not fit in
eBPF: crypto and tunnels (WireGuard, IPsec, QUIC), a userspace TCP/TLS or
app-protocol stack, stateful deep packet inspection, traffic generation, or
anything that needs real Go libraries. The sweet spot is to let XDP cheaply pass
the bulk to the kernel and lift only the flows you care about up to Go.

## Performance

Two bare-metal boxes, AMD EPYC 9275F (24 cores / 48 threads), 100 Gbit/s Mellanox
ConnectX (`mlx5_core`) with 48 combined queues, native XDP and zero-copy on both
ends. [`examples/blast`](examples/blast) on one, [`examples/drop`](examples/drop)
on the other, over a tagged VLAN.

| wire frame | line rate | transmit | receive |
| --- | --- | --- | --- |
| 64 B | 148.8 Mpps | 147.8 Mpps, 99.3 Gbit/s — 16 queues, 16.0 cores | 148.4 Mpps, no drops — 8 queues, 9.3 cores |
| 128 B | 84.5 Mpps | 84.0 Mpps, 99.5 Gbit/s — 16 queues, 16.0 cores | 84.4 Mpps, no drops — 4 queues, 5.6 cores |
| 512 B | 23.5 Mpps | 23.5 Mpps, 100.1 Gbit/s — 4 queues, 4.0 cores | 23.5 Mpps, no drops — 1 queue, 1.3 cores |
| 1500 B | 8.2 Mpps | 8.2 Mpps, 99.9 Gbit/s — 4 queues, 4.0 cores | 8.2 Mpps, no drops — 1 queue, 0.9 cores |

**Line rate in both directions at every frame size**, on one 100G port, with the
defaults and no drops anywhere. Frames are as they appear on the wire, FCS
included; `blast` takes four less than that, since its `-size` excludes the FCS
the NIC appends.

Bigger frames cost less, as they should: the work here is per packet, not per
byte, so a 1500-byte line rate of 8.2 Mpps is eighteen times less work than a
64-byte line rate of 148.8. Receive at 1500 bytes fills the wire with **under a
core**; at 64 bytes it needs 9.3 of 48.

The two sides are measured differently, and the difference matters:

- **Receive cores are work.** The sink blocks in `poll` when there is nothing to
  do, so its CPU is what the packets actually cost. Every processor on the
  machine is counted — userspace and softirq together — which is the honest way
  to measure a path that does half its work in the kernel.
- **Transmit cores are occupancy.** A generator busy-polls: `blast` loops on
  `SendFunc`, so a worker whose transmit ring is full spins, and a spinning core
  is indistinguishable from a busy one in every CPU metric. It occupies one core
  per queue whatever the frame size, which is why the transmit column always
  reads "N queues, N cores". At 1500 bytes those four cores are nearly idle —
  a single queue at 512 bytes already reaches 96.1 Gbit/s, and two at 1500 bytes
  reach 94.5.

**Transmit is not more expensive per packet than receive** — the columns above
only make it look that way. Measured where neither side is spinning, transmitting
a 64-byte frame costs **288 cycles** and receiving one costs **270**: within 7% of
each other.

What differs is how much a single queue can carry. A transmit queue tops out near
15 Mpps, and that is the *queue's* limit rather than the core's — giving one queue
a second core raises it only from 14.1 to 16.2 Mpps. A receive queue carries about
34 Mpps. So the same 148 Mpps needs sixteen queues one way and eight the other,
and since a busy-polling generator occupies a core per queue, the queue count is
what sets the transmit column.

The last fifth is also the expensive part: eight transmit queues already reach
**119.7 Mpps on 8 cores** (14.9 Mpps per core, the same efficiency as receive).
The eight further queues that take it to line rate add only 28 Mpps, because by
then the wire, not the CPU, is what is left to fight.

### Why 128-byte transmit costs more than 64-byte

The table has an oddity worth explaining, because it looks like an error: sending
84 Mpps of 128-byte frames takes the same sixteen queues as 148 Mpps of 64-byte
ones, while receiving them takes four. Bigger frames should be *easier*.

They are — for the wire and for the receiver. It is transmit that has a hole in
it, and it belongs to the driver rather than to this library. Sweeping frame size
on a single transmit queue:

| wire frame | 64 B | 72 B | 96 B | 128 B | 192 B | 256 B | 384 B | 512 B | 1024 B |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Gbit/s | 9.4 | 6.2 | 5.7 | 5.2 | 4.8 | 4.7 | **79.0** | 96.9 | 77.2 |

From roughly 72 to 256 bytes one queue is stuck near 5 Gbit/s, and then jumps
sixteenfold at 384. The reason is that mlx5 copies a small frame *into* the
transmit descriptor instead of pointing at it, so what runs out is the card's
descriptor bandwidth — and that gets worse as the frame grows, which is why the
rate falls with size instead of rising. Above the driver's inline threshold the
descriptor becomes a pointer and the wire takes over again.

That is measurable rather than inferred. Driving the same card through
[packetio](https://github.com/atoonk/packetio), where the descriptor format is
ours to choose, one queue at a time:

| one transmit queue | 64 B | 128 B | 256 B |
| --- | --- | --- | --- |
| packet copied into the descriptor | 16.9 Mpps | **8.8 Mpps** | — |
| descriptor points at the packet | 18.5 Mpps | **18.6 Mpps** | 18.5 Mpps |

Pointing gives a flat packets-per-second ceiling that does not care about frame
size, which is what a queue limit should look like. Copying halves it at 128
bytes.

**Receive has no equivalent**, which is the whole of the asymmetry in the table
above: the card DMAs straight into a buffer the driver posted in advance, so
nothing is copied per frame and the frame size costs nothing.

Copying is not simply the wrong choice, though — it is what makes 64-byte line
rate possible at all. Pointer descriptors cap the *whole card* near 75 Mpps no
matter how many queues you give them, where copying reaches 148. It is only
small fleets and the middle sizes where the trade goes the wrong way — and the
switch turns out to be reachable after all: the driver exposes it as the
`xdp_tx_mpwqe` private flag, and since v0.11.0 `Open` sets it the way the
queue count wants. Four transmit queues or fewer run with pointer descriptors
(17.5 Mpps per core against the copied path's 8.5–14.3, measured at 64- and
68-byte wire frames) and the original state comes back on Close; five or more
keep the kernel default, the only way past the ~75 Mpps wall.
`WithoutMPWQETune` declines, and `Fleet.Info` reports what was chosen. No
ethtool binary is involved — the flag is driven through the ethtool ioctl
directly.

The per-queue table below was taken under different NIC channel counts, so
compare its rows to each other rather than to the single-queue sweeps here.

### Scaling per queue

One socket and one goroutine per queue, so queue count is roughly core count.
Setting the NIC to N queues on both ends with `ethtool -L eno2 combined N`
(reset the RSS table first with `ethtool -X eno2 equal N`, or the change is
refused), 64-byte frames. Cores are **every busy tick on the machine**, with
the softirq share — the kernel's half of AF_XDP — shown beside it, because a
receive path that does half its work in NAPI is not honestly measured any
other way. Medians of three passes with all of today's defaults (placement,
NAPI batching, the descriptor-format choice above):

| queues | sent | TX cores (softirq) | received of 148.8 offered | RX cores (softirq) |
| --- | --- | --- | --- | --- |
| 1 | 18.8 Mpps | 1.0 (0.5) | 32.3 Mpps | 1.6 (1.0) |
| 2 | 35.9 Mpps | 2.0 (1.0) | 65.1 Mpps | 3.2 (2.0) |
| 4 | 71.1 Mpps | 4.0 (2.1) | 128.3 Mpps | 6.5 (4.0) |
| 8 | 120.3 Mpps | 8.0 (5.5) | **148.8 Mpps** | 11.7 (7.7) |
| 16 | **147.3 Mpps** | 16.0 (9.6) | 140.4 Mpps | 11.6 (7.2) |

Transmit runs pointer descriptors to four queues (~17.9 Mpps per core), takes
the copied path from six, and reaches line rate at sixteen. Receive takes the
whole wire on eight queues and costs about 1.5 cores per queue — a worker and
most of a soft interrupt; past its line-rate point more queues only spend
more machine (the 16-queue receive row is the same NIC limit costing more).
A transmit worker is busy-polling, so its user half reads as occupancy; the
receive rows block in poll and their cores are real work.

**More queues is not better.** Past 16 nothing gets faster — transmit is at line
rate and receive is at the NIC's limit — but the receiver keeps costing more CPU.
Measured box-wide, 16 queues carried 116 Mpps using 19.5 of 48 cores, while 48
queues carried 119 Mpps using 24.2 — noticeably more machine for 2% more
throughput.

### NAPI batching (done for you)

Left alone, the kernel wakes an AF_XDP receiver once per NAPI poll, and at these
rates that is millions of times a second — far more often than it needs to. The
receiver then burns its CPU on syscall entry and poll machinery instead of on
packets: profiling the 48-queue sink put `os_xsave`, `sock_poll`, `eventfd_poll`
and interrupt dispatch at the top, with the functions that actually move packets
(`__xsk_map_redirect`, `__xsk_rcv_zc`) nowhere near it.

**`Open` fixes this for you.** On a native-mode NIC it sets
`napi_defer_hard_irqs=2` and `gro_flush_timeout=20µs` so the kernel lets packets
accumulate, and restores your previous values on `Close`. Taking everything on the
wire from a sender offering 132 Mpps of 64-byte frames, across 48 queues:

| | packets per poll | polls/s | kept | box busy |
| --- | --- | --- | --- | --- |
| kernel default | 21 | 5.4M | 114.7 Mpps | 29.4 of 48 cores |
| what `Open` applies | 53 | 2.5M | **130.6 Mpps** | **14.1 of 48 cores** |

More throughput for **half the machine**, which is why this is on by default
rather than a tuning tip. `Fleet.Info()` reports it (`napi defer=2 flush=20µs`)
so it is never invisible.

**The flush timeout is a speed limit, so keep it short.** It is not only a
batching hint: a queue delivers about a thousand packets per flush interval and
no more, whatever is offered — roughly 5 Mpps per queue at 200µs, which is a
third of what one queue can actually do. Spread thin across 48 queues that
ceiling is invisible; concentrated on 8 it is brutal. Same NIC, same 132 Mpps
offered, packets kept and whole-machine cores:

| flush | 8 queues | 16 queues | 24 queues | 48 queues |
| --- | --- | --- | --- | --- |
| none | 132.2, 9.9c | 132.1, 9.0c | 131.2, 15.6c | 114.7, 29.4c |
| **20µs** | **132.2, 9.2c** | **132.2, 9.7c** | **132.1, 12.6c** | **130.6, 14.1c** |
| 50µs | 123.7, 7.2c | 132.2, 9.2c | 132.0, 11.4c | 130.4, 11.6c |
| 100µs | 78.4, 4.0c | 122.3, 7.3c | 132.0, 10.6c | 130.3, 10.2c |
| 200µs | 43.1, 1.9c | 68.9, 3.9c | 103.4, 8.4c | 130.2, 9.7c |

Every step down that table buys CPU by lowering the ceiling. 20µs puts it near
50 Mpps per queue — above anything a single queue reaches — so it costs nothing
at any queue count while still halving the machine at 48. Versions of this
library before this measurement defaulted to 200µs, which was tuned at 48 queues
and silently capped everything below that.

### Transmit packing

mlx5 packs several packets into one work queue entry and, under load, copies
each into it rather than pointing at it. Copying means the card fetches one
thing per packet instead of two, which is the only way past its roughly 75
million packets a second ceiling for separately fetched packets. But an inlined
64-byte packet takes five of the entry's sixteen-byte segments where a pointer
takes one, so five times fewer packets share an entry and each queue runs at
about half speed. Measured on a ConnectX-6 Dx at 64 bytes, Mpps:

| queues | packing off | packing on |
| ---: | ---: | ---: |
| 1 | **17.6** | 8.5 |
| 4 | **69.8** | 46.0 |
| 6 | **75.3** | 69.2 |
| 8 | 75.4 | **92.5** |
| 16 | 74.2 | **140.8** |

Off is twice as fast per queue and stops dead at the port's ceiling; on is half
as fast per queue and has no ceiling. Neither is right on its own, and the
library does not choose for you: the ceiling turned out to be a property of the
adapter rather than of the link, so a rule measured on one card would be a guess
on another. If you drive **six queues or fewer** on an mlx5 NIC and want small
frames, it is worth trying:

    ethtool --set-priv-flags <iface> xdp_tx_mpwqe off

and putting it back afterwards. Above that queue count, leave it on.

Worth knowing, since these are properties of the interface rather than of your
process:

- They are restored on `Close`, but **a crash leaves them applied**. To reset by
  hand: `echo 0 > /sys/class/net/<iface>/napi_defer_hard_irqs` and the same for
  `gro_flush_timeout`.
- Deferring can hold a packet for up to the flush timeout when traffic is sparse,
  so it trades a little idle latency for a lot of loaded throughput.
- Generic/SKB mode is never touched, so veth and test setups are unaffected.
- Opt out with `WithoutAutoTune()`, or change the values with
  `WithNAPITuning(deferIRQs, flush)`. Raise the flush only if you know your
  per-queue packet rate stays under the ceiling above: with many thin queues a
  longer flush is free CPU, and with a few busy ones it is a cliff.

Interrupt coalescing (`ethtool -C rx-usecs`) does *not* substitute for this —
most wakeups here are NAPI flushes rather than hardware interrupts, so raising
`rx-usecs` changed nothing at all.

One curiosity this explains: untuned, throughput becomes oddly sensitive to how
long the XDP program takes, and a filter that reads packet headers batches better
— and so runs *faster* — than one that does nothing. Tuned, that inversion
disappears and the simplest program is the cheapest, as it should be.

### CPU affinity (done for you)

A queue is two halves. The kernel half — the hardware interrupt, the NAPI poll,
your XDP program, the redirect into the socket — runs wherever that queue's
interrupt is routed. The userspace half — your goroutine calling `Poll`,
`Receive`, `SendFunc` — runs wherever the Go scheduler happens to put it. Left
alone the two land on unrelated cores, and every packet then crosses a cache
boundary that it did not have to.

On a chiplet CPU that is expensive. One transmit worker, one queue, interrupt on
core 0, worker moved by hand (EPYC 9275F: 24 cores in L3 complexes of 3, SMT
siblings *n* and *n*+24):

| worker on | | Mpps | cores |
| --- | --- | --- | --- |
| core 1 | same complex, another core | **16.3** | 2.0 |
| core 24 | the interrupt's SMT sibling | 16.2 | 2.0 |
| core 0 | the interrupt's own core | 15.0 | 1.0 |
| core 12 | a different complex | 6.2 | 2.0 |

Leaving the complex costs 2.6× the throughput for the same CPU — and on a
24-core chiplet CPU an unplaced goroutine lands outside the right complex almost
every time.

**`Open` places them for you.** It finds each queue's interrupt and locks the
goroutine driving that queue to a core beside it — the first time that goroutine
calls `Poll`, `Receive`, `ReceivePackets`, `Transmit`, `SendFunc` or `SendBatch`,
which is the moment it identifies itself as that queue's worker. Interrupts with
no free core beside them are moved to one, and put back on `Close`.

A **generator** — `WithFilter(MatchNone())`, which never receives — is placed
*on* its interrupts instead, for the reason below. Transmitting 64-byte frames,
whole-machine CPU, everything else identical:

| queues | before | after |
| --- | --- | --- |
| 1 | 6.5 Mpps, 2.0 cores | **14.3 Mpps**, 1.0 core |
| 4 | 33.0 Mpps, 8.0 cores | **59.6 Mpps**, 4.0 cores |
| 8 | 55.2 Mpps, 16.0 cores | **118.7 Mpps**, 8.0 cores |
| 16 | 112.2 Mpps, 32.1 cores | **147.8 Mpps**, 16.0 cores |
| 24 | 145.9 Mpps, 45.9 cores | 148.5 Mpps, 24.1 cores |

Twice the packets for half the CPU, 1300 cycles per packet down to 290, and at
16 queues 99.3 Gbit/s — 64-byte line rate on sixteen logical CPUs, which is
eight physical cores, or **18.5 Mpps per physical core**. This library did not
reach line rate at all before. The run-to-run spread goes too: the unplaced arm
wandered between 12.9 and 31.0 Mpps at two queues depending on where the
scheduler happened to put things, the placed one between 29.4 and 29.5.

Receiving, with a sender offering 132 Mpps of 64-byte frames the receiver is
free to drop (that was the generator's ceiling, not the receiver's — fed a full
148.7 Mpps the placed receiver keeps 148.5 of it on 9.4 cores, dropping nothing):

| queues | before | after |
| --- | --- | --- |
| 1 | 34.0 Mpps kept, 1.5 cores | 35.5 Mpps, 1.5 cores |
| 2 | 65.2 Mpps kept, 3.1 cores | 66.6 Mpps, 3.2 cores |
| 4 | 125.9 Mpps kept, 6.2 cores | **129.5 Mpps**, 6.3 cores |
| 8 | 45.6 Mpps kept, 2.2 cores | **129.6 Mpps**, 9.2 cores |
| 16 | 66.8 Mpps kept, 3.7 cores | **132.0 Mpps**, 9.6 cores |
| 24 | 107.9 Mpps kept, 10.6 cores | **132.0 Mpps**, 12.6 cores |
| 48 | 129.9 Mpps kept, 10.3 cores | 130.5 Mpps, 13.8 cores |

Everything offered, with **no drops at all**, from four queues up. (These arms
also carry the flush-timeout change above, which is what the 8- and 16-queue
rows are mostly showing; at 48 queues the old 200µs was the cheaper of the two,
which is why it was chosen there.)

**Beside, not on top — unless you only transmit.** Sharing the interrupt's core
is the best possible arrangement for transmit, and it is what the generator
numbers above are doing: one core carries a whole queue instead of half of one.
But it is ruinous for receive, where the kernel half needs a whole core to
itself at 30 Mpps and a receive loop asked to share with it gets almost nothing:

| one receive queue, worker | kept | cores |
| --- | --- | --- |
| beside the interrupt | 30.5 Mpps | 1.50 (1.00 softirq) |
| on the interrupt's core | 0.99 Mpps | 1.01 (0.99 softirq) |

So a fleet that can receive never shares, and its preference order is: same
complex, another physical core first, then the interrupt's SMT sibling, then
nothing at all. A queue with nowhere to go is left to the scheduler, because
stacking it on its own interrupt is worse than not placing it. Only a fleet that
redirects nothing — `MatchNone()`, which cannot receive — shares.

Worth knowing:

- Interrupt routing is host state, like the NAPI settings: restored on `Close`,
  but **a crash leaves it applied**. To reset by hand, restart `irqbalance`, or
  write the driver's own hints back with `/proc/irq/<n>/smp_affinity_list`.
- The goroutine stays locked to its OS thread. Don't drive a socket from a
  goroutine that does anything else of substance.
- A goroutine driving several queues pins once, for the first of them, instead
  of being dragged between them.
- If the kernel refuses the placement — a `taskset` or cpuset that excludes the
  CPU — that is taken as the operator being more specific than the library, and
  the queue is left alone.
- Generic/SKB mode is never placed, so veth and test setups are untouched.
- Opt out with `WithoutAffinity()`, or name the CPUs yourself with
  `WithAffinity(4, 6, 8)` — queue *q* runs on `cpus[q % len(cpus)]` and its
  interrupt is brought to a core beside it.
- `Fleet.Affinity()` returns the CPU per queue (-1 for unplaced), and
  `Fleet.Info()` summarises it.

Driver interrupt names are recognised for `mlx5`, `ixgbe`, `i40e`/`ice`, `bnxt`,
`ena`, `igb`, `mlx4` and `virtio-net`. On anything else no per-queue interrupt is
found, nothing is placed, and `Info` says so.


On a machine with more than one NUMA node, `Open` also registers each queue's
UMEM from a thread bound to that queue's worker CPU: the kernel pins UMEM pages
on the node of the thread that registers them, and that is the memory the NIC
writes every received frame into, so the frames live where the worker runs.

### Queue count

Tune this on the NIC, not in the application. Reduce the channel count so RSS only
spreads over the queues you want, and keep binding all of them (the default):

```
ethtool -X eno2 equal 16      # shrink the RSS table first
ethtool -L eno2 combined 16   # then the channels
```

Using `WithQueues(16)` while the NIC still has 48 RSS queues does something quite
different and usually wrong: the flows hashed to the other 32 queues have no socket
bound, so the XDP program passes them to the kernel and your application never sees
them. You cannot choose which queue a flow lands on, which is why binding every
available queue is the default.

## Filtering

A filter decides which packets are handed to your sockets. Only matching packets
go to userspace; everything else continues to the normal kernel stack. That is
what lets you run on a live interface without stealing SSH or unrelated traffic,
and it is why `Open` requires one.

The shorthand for UDP:

```go
fleet, _ := afxdp.Open("eth0", afxdp.WithUDPPorts(4789)) // VXLAN, say
```

For anything richer, `WithFilter` takes a set of **matches**, and a packet is
redirected if it satisfies **any** of them (logical OR):

```go
// WireGuard on two ports, plus let ping through:
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchUDPPort(51820, 51821),
    afxdp.MatchICMPv4Echo(),
))

// A VXLAN tunnel endpoint and its BGP session:
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchUDPPort(4789),
    afxdp.MatchTCPPort(179),
))

// Replies rather than requests — match the source port:
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchUDPSrcPort(53),  // DNS answers
    afxdp.MatchTCPSrcPort(443), // TLS server -> client
))

// All GRE and all ESP (IPsec), regardless of ports. The protocol matchers are
// per-family, because IPv6 Next Header is not the same question as the IPv4
// protocol field — see the note below:
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchIPv4Proto(47),      // GRE over IPv4
    afxdp.MatchIPv6NextHeader(47), // GRE over IPv6
    afxdp.MatchIPv4Proto(50),      // ESP over IPv4
))

// Ping, both families:
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchICMPv4Echo(),
    afxdp.MatchICMPv6Echo(),
))

// A whole address family, by EtherType:
afxdp.Open("eth0", afxdp.WithFilter(afxdp.MatchEtherType(afxdp.EtherTypeIPv6)))
afxdp.Open("eth0", afxdp.WithFilter(afxdp.MatchEtherType(afxdp.EtherTypeARP)))

// Anything to or from a subnet, by CIDR (IPv4 or IPv6):
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchSrcIP("10.0.0.0/8"),
    afxdp.MatchDstIP("10.0.0.0/8"),
))
afxdp.Open("eth0", afxdp.WithFilter(afxdp.MatchDstIP("2001:db8::/32")))

// Everything except one noisy host — exceptions win over the filter:
afxdp.Open("eth0",
    afxdp.WithFilter(afxdp.MatchAll()),
    afxdp.WithExcept(afxdp.MatchSrcIP("192.0.2.10/32")),
)

// One flow, src AND dst (both directions, OR the two halves):
afxdp.Open("eth0", afxdp.WithFilter(
    afxdp.MatchFlow("10.0.0.1/32", "10.0.0.2/32"),
    afxdp.MatchFlow("10.0.0.2/32", "10.0.0.1/32"),
))
```

Match builders:

| Builder | Matches |
|---------|---------|
| `MatchUDPPort(ports...)` | UDP to these dest ports, IPv4 **and** IPv6 (no ports = all UDP) |
| `MatchUDPSrcPort(ports...)` | UDP from these source ports — replies rather than requests |
| `MatchTCPPort(ports...)` | TCP to these dest ports, IPv4 **and** IPv6 (no ports = all TCP) |
| `MatchTCPSrcPort(ports...)` | TCP from these source ports |
| `MatchIPv4Proto(proto)` | IPv4 with this protocol number (47 GRE, 50 ESP, ...) |
| `MatchIPv6NextHeader(nh)` | IPv6 whose Next Header is this — see the note below |
| `MatchICMPv4Echo()` / `MatchICMPv6Echo()` | echo request (ping / ping6) |
| `MatchSrcIP(cidr)` | source IP in this CIDR, IPv4 or IPv6 (e.g. `10.0.0.0/8`, `2001:db8::/32`) |
| `MatchDstIP(cidr)` | destination IP in this CIDR, IPv4 or IPv6 |
| `MatchFlow(src, dst)` | src CIDR **and** dst CIDR together, i.e. one direction of a flow |
| `MatchEtherType(et)` | this EtherType (`0x0806` ARP, `0x86DD` IPv6, ...) |
| `MatchAll()` | every packet, the deliberate "take everything" |
| `MatchNone()` | nothing, attach without redirecting (e.g. zero copy TX for a sender) |

That is every built-in builder. For anything they miss,
[`bpfmatch`](#filtering-with-tcpdump-expressions) matches whatever a tcpdump
filter matches, and [`NewMatch`](#custom-matches) takes raw eBPF.

The options that install them, and the two that hold traffic back, are:

| Option | Effect |
|--------|--------|
| `WithFilter(matches...)` | redirect packets matching **any** of these builders |
| `WithUDPPorts(ports...)` | shorthand for `WithFilter(MatchUDPPort(ports...))` |
| `WithExcept(matches...)` | pass packets matching any of these to the kernel, whatever the filter says |
| `WithKeepManagement(extraTCPPorts...)` | the inverse: keep ARP, IPv6 ND, SSH and DNS replies *out* of the capture so a broad filter cannot lock you out of the box ([below](#keeping-your-session-alive-withkeepmanagement)) |
| `WithProgram(build)` | attach your own XDP program instead of a filter: `Open` gives `build` the XSK map and the queue map, and attaches and closes the program it returns as its own. It can drop, which a filter cannot |

Each match is compiled to eBPF instructions with
[`github.com/cilium/ebpf/asm`](https://pkg.go.dev/github.com/cilium/ebpf/asm) into
a single XDP program, loaded and checked by the kernel verifier (the test suite
loads every builder and a composite to prove they verify).

A few things to know. Matches combine with OR, a packet is redirected if it
matches any of them. The one built-in AND is `MatchFlow`; for arbitrary AND, and
for anything else these builders miss, use
[`bpfmatch`](#filtering-with-tcpdump-expressions).

The port matchers handle IPv4 **and** IPv6. The protocol and echo matchers are
named for the family they match, because the two are not equivalent: IPv6's Next
Header names whatever comes next, which may be an extension header rather than
the upper-layer protocol, and ICMP and ICMPv6 are different protocols with
different numbers.

All of the port, protocol and echo matchers assume no IP options and no IPv6
extension headers, so a packet carrying either does not match. The
[cBPF layer](#filtering-with-tcpdump-expressions) is what handles those, because
pcap-compiled filters compute the header length instead of assuming it. The IP
(CIDR) matchers read fixed offsets and are unaffected by both.

Every matcher transparently skips a single 802.1Q VLAN tag, so the same filter
works whether or not the NIC strips the tag before XDP — stacked QinQ tags are
not unwound.

### Filtering with tcpdump expressions

`bpfmatch.Match` matches packets accepted by a **classic BPF** program — the
instruction set `tcpdump` and libpcap compile their filter expressions to. It is
compiled to eBPF with [`cbpfc`](https://github.com/cloudflare/cbpfc) and spliced
into the filter alongside every other match, so a tcpdump filter expression
becomes an in-kernel filter:

```
tcpdump -ddd 'tcp port 22 and not src host 192.0.2.1'
```

The usual way in is the expression itself, via the optional `pcapfilter`
module:

```
go get github.com/atoonk/go-afxdp/pcapfilter
```

```go
fleet, err := afxdp.Open("eth0", afxdp.WithFilter(
    pcapfilter.Match("tcp port 443 and not src net 192.0.2.0/24"),
), afxdp.WithKeepManagement())
```

A runnable version is in
[`pcapfilter/example`](pcapfilter/example).

Underneath it, `bpfmatch` takes the compiled **classic BPF instructions**
directly, with no libpcap and no cgo:

```
go get github.com/atoonk/go-afxdp/bpfmatch
```

```go
// insns is []bpf.Instruction, e.g. the output of
//   tcpdump -ddd 'tcp port 22 and not src host 192.0.2.1'
//
// The first argument is only a label: it names the rule in Fleet.Info and in
// error messages, and has no effect on what matches.
 fleet, err := afxdp.Open("eth0", afxdp.WithFilter(
    bpfmatch.Match("tcp/22 except 192.0.2.1", insns),
 ))
```

#### Which one should I use?

`pcapfilter` is the easier of the two and the right default: you type the
expression you already know. Reach for `bpfmatch` when one of these applies.

| | `pcapfilter` | `bpfmatch` |
|---|---|---|
| You write | `"tcp port 22"` | `[]bpf.Instruction` |
| Needs libpcap + cgo | yes | **no** |
| `CGO_ENABLED=0` / static binary | **does not build** | builds |
| Cross-compile, scratch/distroless image | no | yes |

The deciding question is usually **do you ship binaries?** A static or
cross-compiled build has no cgo, so `pcapfilter` is not available there at all —
that is why Wireblast, which ships binaries users download, cannot require it.
The other cases for `bpfmatch`: compiling the expression somewhere else (build
host or control plane) and shipping only the instructions to the data plane, or
cBPF that never came from a string in the first place — generated by a
controller, stored in a config, or produced by `tcpdump -ddd` in a script.

They are the same engine. `pcapfilter` calls libpcap to turn your string into
classic BPF and hands those instructions straight to `bpfmatch`, so both produce
identical eBPF and go through identical validation. The only difference is who
parses the expression, and what that costs you at build time.

Either way, this is the layer to reach for before writing eBPF by hand. Any
normal packet-data expression — anything you would type after `tcpdump` —
works:


| Expression | Captures |
|---|---|
| `tcp port 22 and not src host 192.0.2.1` | SSH, except from one host — `and`/`not`, which `WithFilter` alone cannot express |
| `udp portrange 5000-6000` | a port range, without unrolling a thousand compares |
| `tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack = 0` | connection openers only |
| `vlan 100 and tcp port 443` | one VLAN, and inside it one port |
| `ip6 and tcp port 443` | a single address family — like the built-ins, this reads Next Header and does not walk extension headers; `protochain 6` does |
| `proto gre` / `ip proto 47` | GRE, either spelling |
| `icmp or icmp6` | all ICMP and ICMPv6 traffic — errors, ND and echo alike |
| `ether dst 01:00:5e:00:00:01` | one destination MAC, multicast included |
| `ip[6:2] & 0x1fff != 0` | IPv4 fragments — an arbitrary byte offset with a mask |
| `udp port 4789 and udp[12:4] & 0xffffff00 = 0x0004d200` | one VXLAN VNI — 1234 is `0x0004d2`, in the top three of the four loaded bytes |
| `greater 1000` | frames over 1000 bytes |

Unlike the named builders it also handles **IPv4 headers carrying options**,
because pcap-compiled filters compute the header length rather than assuming
it.

One caveat for cBPF from *other* sources: the program must stick to packet
data. Filters compiled against a live capture handle can contain Linux
ancillary loads (`SKF_AD_*`, e.g. for the kernel-stripped VLAN tag) that read
socket-buffer metadata XDP does not have; `bpfmatch` rejects those with an
error rather than mismatching, but the message names a negative offset you
never wrote. `pcapfilter`, `tcpdump -ddd`, and anything else that compiles
against a dead handle never produces them.

Semantics are exactly those of the cBPF program you supply, evaluated against
the frame as XDP received it. Note XDP is ingress-only, so a filter written
expecting both directions of a conversation only sees the inbound half.

Both are **separate modules**, so the core `go-afxdp` module stays pure Go: no
cgo, no libpcap, no `tcpdump` binary, and none of the newer Go or `cilium/ebpf`
versions the cBPF compiler needs. You take those on only by importing the layer
that uses them.

### Custom matches

If neither the named builders nor the cBPF layer cover what you need, `NewMatch`
lets you emit your own eBPF classification block. Most people should not need
it — reach for `bpfmatch`/`pcapfilter` first. A custom block gets the same
treatment as the built-in ones: assembled into the filter program, checked by
the verifier, and reported by `Fleet.Info`.

The builder receives a `MatchEnv` and returns instructions that jump to
`env.Redirect` on a match and `env.Next` otherwise. This one reimplements
`MatchUDPPort(5000)`:

```go
udp5000 := afxdp.NewMatch("udp/5000", func(e afxdp.MatchEnv) (asm.Instructions, error) {
    ins, frame := e.FrameBase()
    // Ethernet 14 + IPv4 20 (no options) puts the UDP dest port at 36.
    ins = append(ins, e.Bounds(frame, 36+2)...)
    return append(ins,
        asm.LoadMem(asm.R3, frame, afxdp.OffEtherType, asm.Half),
        asm.JNE.Imm(asm.R3, afxdp.NetShort(afxdp.EtherTypeIPv4), e.Next),
        asm.LoadMem(asm.R3, frame, 23, asm.Byte), // IP protocol
        asm.JNE.Imm(asm.R3, 17, e.Next),          // UDP
        asm.LoadMem(asm.R3, frame, 36, asm.Half), // UDP dest port
        asm.JEq.Imm(asm.R3, afxdp.NetShort(5000), e.Redirect),
    ), nil
})

fleet, err := afxdp.Open("eth0", afxdp.WithFilter(udp5000))
```

`e.FrameBase()` returns the instructions that establish a frame base with a
single VLAN tag skipped, plus the register holding it — so a custom match
inherits the same tag handling as the built-ins. `e.Bounds(base, n)` emits the
bounds check the verifier requires before any packet read; skip it and the
program is rejected. Falling off the end of the block means "no match", so you
only need `e.Next` for early exits. `e.Label(name)` gives you a symbol unique to
your block if you need your own control flow. A builder that cannot fail
returns a nil error; the error exists for builders that compile something, as
`bpfmatch` does.

Registers, which matter because the block is spliced into a larger program:

| Register | Role | Your block may |
|---|---|---|
| `R6` (`e.DataEnd`) | `data_end` | read |
| `R7` (`e.Data`) | frame start | read |
| **`R8`** | **`rx_queue_index`** | **nothing — reserved** |
| `R9` | the `FrameBase()` register | use once `FrameBase()` has been called |
| `R0`–`R5` | scratch | use *between* helper calls |

`R8` is the one that used to be dangerous: the redirect tail reads it to pick
the destination socket, so a block that overwrote it verified cleanly, attached,
and then delivered packets to the wrong queue with no error anywhere. `NewMatch`
now rejects any block that mentions `R8` at all, so that mistake is an error from
`Open` instead.

`Bounds` and `FrameBase` use scratch registers internally, and which ones is not
part of the contract — don't assume a value in `R0`–`R5` survives a call to one.

Two failure modes worth knowing: a filter in which *no* match can ever reach
`Redirect` leaves the redirect path unreachable, which the verifier rejects; and
under `WithMultiBuffer` a read past the first fragment still loads but silently
stops matching, so keep reads inside the L2/L3/L4 headers.

Verifier rejections come back from `Open` as an `*ebpf.VerifierError`. Printing
it with `%v` gives only its first line (`unreachable insn 6`); the program
listing that shows which instruction was rejected needs the concrete type:

```go
fleet, err := afxdp.Open("eth0", afxdp.WithFilter(myMatch))
if err != nil {
    var ve *ebpf.VerifierError
    if errors.As(err, &ve) {
        log.Fatalf("filter rejected:\n%+v", ve) // %+v, and only on ve
    }
    log.Fatal(err)
}
```

### Testing a custom match

`MatchPacket` runs a filter against a packet you supply and reports whether it
would be redirected. It assembles and executes the same eBPF `Open` would
attach — it does not reimplement matching in Go — so a wrong offset, a missed
byte swap or a mishandled VLAN tag shows up exactly as it would on a live NIC:

```go
ok, err := afxdp.MatchPacket(frame, afxdp.WithFilter(udp5000))
```

It takes the same options `Open` does, so it can model the filter you actually
deploy — exceptions included:

```go
ok, err := afxdp.MatchPacket(frame,
    afxdp.WithFilter(udp5000),
    afxdp.WithExcept(afxdp.MatchSrcIP("192.0.2.10/32")),
)
```

That makes a custom match unit-testable with no NIC and no traffic. Test the
near misses too — a byte-order slip usually still matches *something*, so a
matcher only ever shown packets it should accept looks correct until it ships.
It needs the same privileges as `Open` (`CAP_BPF` and `CAP_NET_ADMIN`, or root).
Do not blanket-skip on error, though: the kernel reports a verifier rejection as
`EACCES`, so a broken matcher and a missing capability look identical. Unwrap to
`*ebpf.VerifierError` and fail on that; skip on anything else.

Worked examples live in [`examples/customfilter/`](examples/customfilter).
Start with [`udpsrcport`](examples/customfilter/udpsrcport), which is the
shortest and exists purely as a tutorial — for real source-port matching use the
built-in `MatchUDPSrcPort`. The other four each capture something no built-in
expresses: a [VLAN ID](examples/customfilter/vlan), a
[destination MAC](examples/customfilter/dstmac),
[TCP SYNs](examples/customfilter/tcpsyn), and a
[VXLAN VNI](examples/customfilter/vxlan).

For the tcpdump-expression path — the one most people should reach for — see
[`pcapfilter/example`](pcapfilter/example).

### Keeping your session alive: `WithKeepManagement`

`MatchAll()` on the NIC you are logged in through takes every packet away from the
kernel, including the ones carrying your session. `WithKeepManagement()` leaves
those with the kernel and captures the rest:

```go
fleet, err := afxdp.Open("eth0",
    afxdp.WithFilter(afxdp.MatchAll()), // capture everything...
    afxdp.WithKeepManagement(),         // ...except what keeps me logged in
)
```

What stays with the kernel:

| passed through | why |
| --- | --- |
| ARP, IPv6 ND (ICMPv6 133–137) | gateway MAC resolution |
| TCP to/from port 22, addressed to this interface | inbound and outbound SSH |
| UDP and TCP source port 53, addressed to this interface | DNS replies |

**ARP matters more than the SSH rule.** Pass SSH through but swallow ARP and the
box still goes dark: the kernel cannot refresh the gateway's link-layer address,
and about a minute later it can no longer reply to anything. That is the failure
this exists to prevent, and it is why the preset is not just "port 22".

The port rules are scoped to the addresses the interface has when `Open` is
called, so a router still captures transit traffic on port 22 — only traffic
addressed to this box is spared. Every address is covered, IPv4 and IPv6, however
many of each: a rule is emitted per address, and the address family selects the
header offsets. Addresses added later are not covered; reopen the fleet if they
change. Pass extra TCP ports for SSH on a non-standard port:
`WithKeepManagement(2222)`.

Two things to know. Traffic *from* port 22 or 53 to this host is not captured, so
a sender that picks those source ports evades the capture — irrelevant for
measurement, relevant if you are hunting an adversary. And if you administer the
box through a different NIC than the one you are capturing on, you do not need
this at all.

## Transmit

The easy way is `SendBatch` (copy your buffers in) or `SendFunc` (fill each frame
in place, no copy, ideal for a generator that varies a field per packet). Both
handle all the ring bookkeeping, so you just call them in a loop.

```go
// SendFunc fills each frame in place and returns the packet length.
for {
    _, err := xsk.SendFunc(256, func(i int, frame []byte) int {
        n := copy(frame, template)
        // offset 34 is the UDP source port (eth 14 + ip 20); vary it per packet
        binary.BigEndian.PutUint16(frame[34:], srcPort)
        srcPort++
        return n
    })
    if err != nil {
        // A length > FrameSize is a caller bug. Don't crash (or log unbounded)
        // inside a dataplane loop — log it rate-limited and keep going; see
        // the errLog helper in examples/blast.
    }
}
```

If you want full control, the primitives underneath are exported too,
**Alloc, build, Transmit, Complete**, plus `Kick` and `NumFreeTxSlots`. The one
rule if you hand-roll the loop: when the ring is full, still call `Kick`, or
copy-mode TX deadlocks (the kernel will not drain it on its own). `SendBatch` and
`SendFunc` handle that for you.

[`examples/blast`](examples/blast) is a line-rate generator built on `SendFunc`.

### Transmit metadata: let the NIC finish checksums

Linux 6.8 added a small header the kernel reads from the bytes just before a
transmit frame, through which you ask the NIC to finish a transport checksum
(or timestamp the frame). Turn it on with `WithTxMetadata()`: every transmit
chunk is then laid out as 16 bytes of metadata followed by the packet, `Alloc`
and `SendFunc` hand you the packet part (`FrameSize` minus 16 bytes), and the
descriptor says the metadata is there. A frame that asks for nothing is sent
exactly as before, so turning it on costs nothing until you use it.

```go
feat, _ := afxdp.QueryXSKFeatures("eth0") // does the driver honour it?
fleet, err := afxdp.Open("eth0", afxdp.WithTxMetadata(), ...)

xsk.SendFunc(n, func(i int, frame []byte) int {
    n := buildTCPSegment(frame) // checksum field = pseudo-header sum only
    afxdp.TxMetadata{
        Flags:      afxdp.TxMetaChecksum,
        CsumStart:  14 + 20, // start summing at the TCP header
        CsumOffset: 16,      // where in it the checksum field is
    }.Put(xsk.TxMeta(frame))
    return n
})
```

The contract is Linux's own `CHECKSUM_PARTIAL`: the checksum field holds the
un-complemented pseudo-header sum, and the NIC sums from `CsumStart` and
writes the result at `CsumStart+CsumOffset`. Ask first with `QueryXSKFeatures`
(the netdev generic-netlink family, Linux 6.3+): a driver that does not
implement the request transmits the frame untouched, with a wrong checksum and
no error anywhere. On this tree's kernel ixgbe does exactly that; mlx5 honours
it. A copy-mode socket (veth, generic XDP) computes the checksum in software,
so a program behaves the same in tests as on a NIC that offloads.

`WithTxMetadata` cannot be combined with `WithTxReuseRxFrames` (received frames
have no room in front of them), and `SendBatch` payloads are capped at
`FrameSize` minus the 16 bytes.

### One UMEM for the whole fleet: `WithSharedMemory`

By default each socket maps its own UMEM, so a frame address means something
only to the socket it belongs to. `WithSharedMemory(extra)` maps one region
for the fleet — every queue's frames in a row, then `extra` bytes the fleet
never touches — and registers that same region with each socket, the way VPP
registers its buffer pool with every AF_XDP socket it opens. Each socket still
owns its own frames (its fill ring and transmit pool draw only from its
slice), but an address is the same on every socket, and a frame in the extra
area can be transmitted by any of them without a copy:

```go
fleet, _ := afxdp.Open("eth0", afxdp.WithSharedMemory(256<<20),
    afxdp.WithOnForeignComplete(func(addr uint64) { cache.release(addr) }))
base, mem := fleet.Extra()          // the caller's area and its address
copy(mem[off:], packet)             // build the frame once, in place
xsk.Transmit([]afxdp.Desc{{Addr: base + off, Len: uint32(len(packet))}})
```

A transmitted address from outside a socket's own frames is reported to
`WithOnForeignComplete` when it completes, from inside `Complete`, instead of
going into a pool — so keep that callback to an atomic and never call back
into the socket from it. `Fleet.Memory()` is the whole region. The same rules
as any frame apply to what you build there: a descriptor stays within one
`FrameSize`-aligned chunk, and with `WithTxMetadata` on it leaves the 16-byte
gap before its address. `examples/netstack` serves a content cache this way.

## Options and XDP mode

Everything is configured with functional options on `Open`:

| Option | Effect |
|--------|--------|
| `WithQueues(n)` | bind n rx queues, from queue 0 (0 or omitted = all) |
| `WithUDPPorts(p...)` | shorthand for `WithFilter(MatchUDPPort(p...))` |
| `WithFilter(m...)` | redirect packets matching any of the given matches |
| `WithNumFrames(n)` | total UMEM buffers, rx + tx (default 4096) |
| `WithFrameSize(n)` | bytes per buffer (default 2048; auto **4096** on ENA for zero copy) |
| `WithTxFrames(n)` | buffers reserved for transmit (default half) |
| `WithRingSize(n)` | all four ring sizes, power of two (default 2048) |
| `WithZeroCopy()` | require native zero copy, `Open` fails if unavailable |
| `WithDriverMode()` / `WithGenericMode()` | force native / generic attach (default: auto) |
| `WithMultiBuffer()` | let packets span several frames — jumbo support, costs zero copy |
| `WithTxReuseRxFrames()` | let a forwarder transmit the frame it received, no copy (see below) |
| `WithOptions(o)` | drop in a full `Options` struct, then override fields |

By default `Open` picks the mode for you. It tries native zero copy, then native
copy, then generic copy, using the first the driver accepts, so you get the fast
path on a real NIC and it still works on veth without you choosing. `Fleet.Info()`
reports what was selected. You rarely need to override it; `WithGenericMode`
forces generic (and never blips the link), `WithDriverMode` forces native, and
`WithZeroCopy` requires zero copy.

Heads up: native XDP reinitializes the driver's rings, so attaching or detaching
it **blips the link**. On some 10G NICs (e.g. Intel ixgbe) the PHY then
renegotiates for several seconds before the carrier is back, during which nothing
can send or receive. So a native-mode program may sit idle for a few seconds at
startup; that is the link relinking, not a hang. (The `blast` example waits for
the link to come up first, for exactly this reason.) `WithGenericMode` does not
reset the link, which is handy for quick local tests.

`WithFrameSize(4096)` gives zero copy on drivers that need page-sized frames;
Open already applies it on AWS ENA (see below), so you rarely set it by hand.
Each socket has its own UMEM of `NumFrames * FrameSize` bytes, so memory scales
with the queue count; size `NumFrames` accordingly on many-queue NICs.

### Zero-copy forwarding: `WithTxReuseRxFrames`

By default the receive and transmit frame pools are disjoint, which is what
lets one receive goroutine and one transmit goroutine run without locking. The
catch: a router that receives a packet and wants to send it back out has to
copy it from an rx frame into a tx frame, because you can only transmit tx-pool
frames.

`WithTxReuseRxFrames()` removes that copy. It makes `Complete` return each
finished frame to the pool its address belongs to (an rx frame goes back to the
rx pool) instead of always the tx pool, so you can `Receive` a frame, rewrite
it in place, `Transmit` the same descriptor, and on completion it flows back to
the fill ring — no copy either direction.

The tradeoff: `Complete` runs on the transmit side and may now touch the rx
pool, so it is only safe when **one goroutine drives both sides** of the socket
(the forwarder shape — receive, process, transmit in one loop). That is the
common router layout, but it is not the default because it relaxes the
lock-free split. It also can't combine with `WithMultiBuffer()` (`Open` refuses
the pair): the completion routing recovers a frame's pool from its address with
aligned-chunk arithmetic that assumes one frame per packet.

```go
xsk, _ := afxdp.Open("eth0", afxdp.WithTxReuseRxFrames())
for {
    xsk.Fill(xsk.NumFreeFillSlots())
    for _, d := range xsk.Receive(64) {
        rewrite(xsk.GetFrame(d))   // edit the packet in place
        xsk.Transmit([]afxdp.Desc{d})
    }
    xsk.Complete(xsk.NumCompleted())
}
```

### Need Wakeup

`WithNeedWakeup()` binds with `XDP_USE_NEED_WAKEUP`, the recommended operating
mode for AF_XDP:

```go
fleet, err := afxdp.Open("eth0",
    afxdp.WithUDPPorts(4789),
    afxdp.WithNeedWakeup(),
)
```

With the flag set, the kernel parks idle RX/TX queues and asks for an explicit
wakeup through the AF_XDP ring flags instead of NAPI-polling in a loop (without
it, a buffer-starved driver can burn entire cores in ksoftirqd while forwarding
nothing — see the `WithNeedWakeup` doc comment for the gory details). The
library handles the waking for you: `Poll` wakes the receive path, and the
transmit path kicks the kernel as needed.

It also cuts transmit syscalls: when the ring flags show the driver awake and
draining (typical in zero-copy mode), `Transmit` skips the `sendto` kick
entirely. `Stats().Kicks` and `Stats().KicksSuppressed` show how often each
happens.

It is not the default only because it changes the kernel contract for
applications that drive the rings themselves rather than through `Poll`/`Kick`.
If you use the high-level API, turn it on.

## AWS EC2 / ENA

The `ena` driver (EC2, including the "network optimized" `*n`/`*gn` instances)
supports native XDP, but the MTU has to be right, and on driver versions before
2.17.0 the channel count too. Miss either and `Open` silently falls back to
**generic** XDP, which works but drops packets on the floor under load without
any counter showing it. `Fleet.Info()` tells you which mode you got; if it says
`generic` on ENA, check these:

1. **Lower the MTU.** Base XDP hands the program one contiguous, page-sized
   (4 KB) buffer per packet, so a 9001-byte jumbo frame doesn't fit and ENA
   rejects the attach. Set the MTU under ~3.5 KB:
   ```
   ip link set dev ens5 mtu 3000
   ```
   (EC2 defaults to jumbo 9001. This is the driver's single-buffer XDP limit,
   not a library choice.) If you actually need jumbo frames, `WithMultiBuffer()`
   plus the driver patch in [contrib/ena-jumbo/](contrib/ena-jumbo/) lifts this —
   at the cost of zero-copy, so lowering the MTU stays the faster option
   otherwise.

2. **Only on ENA older than 2.17.0: free up queues for XDP.** Those versions
   carved a dedicated transmit ring per channel out of the same fixed hardware
   queue budget as your normal channels, and refused a native attach unless
   channels were **≤ half** the maximum. ENA 2.17.0 added full queue
   utilization in XDP and the limit is gone: measured on 2.17.2g, full channels
   (4 of 4) give native zero-copy for both receive and transmit. Check your
   version first, and only halve the channels if it is below 2.17.0:
   ```
   ethtool -i ens5 | grep '^version'
   ethtool -L ens5 combined 2
   ```

**Zero copy** on ENA additionally needs page-sized (4096-byte) UMEM frames — with
the default 2048 the bind silently drops to native *copy* mode. Open handles this
for you: when it sees the `ena` driver it defaults `FrameSize` to 4096, so once
the MTU is right the banner reads `zero-copy, native XDP` with no code
change. (Pass `WithFrameSize` yourself only to override that. It costs twice the
UMEM per queue, which is why 4096 is an ena-only default, not the global one.)

These `ethtool`/`ip` settings are per-boot; re-apply after a reboot. They are NIC
config, so set them yourself rather than have the library reconfigure your
interface underneath you. (The frame-size default is the one thing the library
*can* safely pick for you, since it only changes its own UMEM, not your NIC.)

### Jumbo frames (multi-buffer)

By default a packet must fit one UMEM frame, which is what forces the MTU step
above. `WithMultiBuffer()` lets a packet span several frames instead: it loads
the XDP program with `BPF_F_XDP_HAS_FRAGS` (so it can attach at a jumbo MTU at
all) and binds the socket with `XDP_USE_SG` (without which the kernel silently
drops every multi-buffer packet).

Read chained packets with `ReceivePackets` rather than `Receive` — `Receive`
returns one `Desc` per *frame*, so a jumbo packet looks like several unrelated
descriptors. `SendBatch` splits oversized payloads for you.

```go
fleet, _ := afxdp.Open("ens5",
    afxdp.WithFilter(afxdp.MatchUDPPort(4789)),
    afxdp.WithMultiBuffer(),
)
xsk := fleet.Sockets()[0]

pkts := xsk.ReceivePackets(64)      // []Packet, each a []Desc in wire order
for _, p := range pkts {
    n := xsk.CopyOut(p, buf)        // or walk p's fragments to avoid the copy
    _ = buf[:n]
}
xsk.RecyclePackets(pkts)
```

**The trade-off:** a device reports its multi-buffer zero-copy limit as
`xdp-zc-max-segs`. Where that is 1 — which is every ENA today — the kernel
refuses an `XDP_USE_SG` bind in zero-copy mode, so `Open` settles for native
*copy*. On ENA you get jumbo **or** zero-copy, never both. Native copy still
beats the generic fallback comfortably, but if you don't need jumbo frames,
lowering the MTU and leaving this option off is faster. Check `Info().ZeroCopy`.

**On AWS this needs a driver patch.** A stale compile probe in the ENA driver
disables multi-buffer on current kernels, so XDP still won't attach at MTU 9001
no matter what this library does. A one-line fix, the reasoning behind it, and
step-by-step instructions are in [contrib/ena-jumbo/](contrib/ena-jumbo/).

Measured on two `c7gn.xlarge` (4 vCPU, kernel 6.18, ena 2.17.2g, `blast` → `drop`
over the private subnet, 64-byte frames), showing why the mode matters:

| Receiver mode | MTU | rx pps | CPU per packet |
|---------------|-----|--------|----------------|
| generic XDP (no setup) | 9001 | 4.89M, ~2% loss | — |
| native, copy (`WithMultiBuffer`) | 9001 | 4.29M | 0.375 µs |
| native, copy (`WithMultiBuffer`) | 3000 | 4.96M | 0.349 µs |
| native + zero copy (auto 4096 frames) | 3000 | **5.00M**, 0.16% loss | 0.280 µs |

With the MTU set and the auto-4096 frames giving a `zero-copy, native XDP`
banner, `blast → drop` runs a **clean, steady 5.0M pps** end to end.

Three things that table will mislead you about if you read it too quickly:

- **The ceiling is the instance, not the library.** AWS's Nitro network layer
  **polices packets-per-second**, so past the instance's allowance (~5M pps on
  `c7gn.xlarge`) the `pps_allowance_exceeded` counter in `ethtool -S ens5`
  climbs and the rate flat-lines there regardless of queues, cores or mode.
  Bigger instances raise the allowance.
- **Copy and zero copy therefore look closer than they are.** They reach nearly
  the same pps here only because the allowance binds first. The real difference
  is CPU: copy costs about 1.25× per packet at 64 bytes and 1.76× at 1400 bytes,
  where there is more to copy. That matters when you are CPU-bound, which on
  this instance size you are not.
- **The jumbo MTU costs about 13% pps by itself** (4.29M vs 4.96M, same mode).
  If you turn on `WithMultiBuffer()` but do not actually need jumbo frames, you
  pay that for nothing.

Also worth knowing: **generic used to be far worse**. On a 6.1 kernel this same
test lost roughly 25% of a 4M pps sender. On 6.18 it loses about 2%. Still worth
avoiding, since it drops packets where native does not and cannot do zero copy,
but the old figure no longer describes it.

## Examples

| Example | Shows |
|---------|-------|
| [`examples/helloworld`](examples/helloworld) | the simplest program, `Open` with an ICMP filter, log `Info`, print pings, periodic `Stats` |
| [`examples/drop`](examples/drop) | UDP sink that discards everything, minimal per-packet work, for measuring raw receive pps |
| [`examples/blast`](examples/blast) | UDP packet generator, builds frames in the UMEM and transmits at line rate, the sender to point at `drop` |
| [`examples/l2fwd`](examples/l2fwd) | the low-level API (`NewSocket`/`NewProgram`), reflect frames, per-socket `Stats` |
| [`examples/multiqueue`](examples/multiqueue) | `Open` across all queues, `Info` plus aggregate `Stats` |
| [`examples/udpreflector`](examples/udpreflector) | `Open` plus a UDP-port filter, wire-speed UDP echo with `Info`/`Stats` |
| [`examples/dns`](examples/dns) | a real scenario, a UDP/53 forwarding DNS resolver: AF_XDP client path, `miekg/dns` upstream to 8.8.8.8, async worker pool |
| [`examples/natlb`](examples/natlb) | **a NAT-mode TCP load balancer**: SNAT/PAT plus DNAT in userspace, a connection table both ways, zero-copy rewrite with `WithTxReuseRxFrames`, and a namespace demo you can run |
| [`examples/netstack`](examples/netstack) | **a userspace TCP server at 10G line rate**: gVisor's TCP/IP stack (netstack) fed straight from the rings through a `stack.LinkEndpoint`, with segmentation and receive offload done in the endpoint, an echo/HTTP server that terminates TCP off the kernel, a `-stack kernel` A/B, and the gVisor patches it took |
| [`pcapfilter/example`](pcapfilter/example) | **filtering by tcpdump expression** — `-filter "tcp port 443 and not src net 192.0.2.0/24"` |
| [`examples/customfilter/gre`](examples/customfilter/gre) | **the low-level story end to end**: a custom `Match` written with the exported API, `MatchError` validation, and a unit test that runs it against crafted packets with no NIC |
| [`examples/customfilter/`](examples/customfilter) | four more custom matchers: a VLAN ID, a destination MAC, TCP SYNs, a VXLAN VNI, plus `udpsrcport` as the tutorial |

```
go build -o drop ./examples/drop
sudo ./drop -iface eth0 -port 9999
```

The `dns` example is its own Go module (so only it pulls in `github.com/miekg/dns`
and the core library stays dependency-minimal). Build it from its directory:

```
cd examples/dns && go mod tidy && go build .        # also examples/netstack (its own module; pulls in gVisor)
sudo ./dns -iface eth0 -upstream 8.8.8.8:53
```

## Concurrency

A `Socket` is safe for **one receive goroutine concurrent with one transmit
goroutine, lock-free**. Within a direction it is single-threaded. If multiple
goroutines transmit on one socket, serialize the tx-side calls (`Alloc`,
`Transmit`, `Complete`) with your own mutex; the rx side still needs none. Or give
each producer its own queue. The receive side is single-consumer too.

A common shape is one goroutine per queue handling both directions for that queue,
as in the examples.

## Introspection: `Info` and `Stats`

`Fleet.Info()` reports how the fleet is actually running, handy to log at startup,
and `Fleet.Stats()` aggregates per-queue counters so you do not have to track them
yourself. Both have `String` methods.

```go
info, _ := fleet.Info()
log.Printf("started: %s", info)
// started: eth0: 8 queues, zero-copy, native XDP, 4096x2048B frames, driver ena, filter udp/4789

s, _ := fleet.Stats() // e.g. once a second
log.Print(s)
// rx=1530244 tx=0 packets, 19 pkt/poll, rx_drops=12
```

`Info` exposes the interface, NIC driver, queue count, frame size and count, the
XDP attach mode (native, generic, or hardware, read back from the kernel),
whether zero copy was actually granted (read from each socket's `XDP_OPTIONS`, not
just what was requested), and the applied **filter** as a readable summary
(`udp/53`, `udp/4789 | icmp-echo`, or `all` when nothing is filtered).

`Stats` sums received and transmitted packet counts (straight from the rings, no
per-packet bookkeeping in your loop) and the kernel's drop and error counters
(`rx_dropped`, `rx_ring_full`, invalid descriptors), with a `PerQueue` breakdown
when you need to find a hot or dropping queue. All counters are cumulative, so
sample twice and subtract for a rate. Byte counts are not included, the kernel
does not track them, so sum frame lengths in your loop if you need them.

### Syscall counters

Three counters expose what the library is doing with syscalls, which is usually
what you want when a loop is slower than expected:

| field | meaning |
| --- | --- |
| `Polls` | blocking `poll(2)` calls on the receive side |
| `Kicks` | `sendto(2)` kicks issued on the transmit side |
| `KicksSuppressed` | transmit kicks skipped because need-wakeup showed the driver awake |

`Stats.PacketsPerPoll()` divides `Received` by `Polls`: how many packets each
receive syscall paid for, i.e. how well your loop batches. A `drop` sink at
12 Mpps over 12 zero-copy ixgbe queues measures about 20 packets per poll; a
value near 1 means a syscall per packet, usually because the loop drains less
per wakeup than is waiting. `KicksSuppressed` climbing on a zero-copy link means
[need-wakeup](#need-wakeup) is doing its job.

```go
s, _ := xsk.Stats()
log.Printf("%.0f pkt/poll, %d polls, %d kicks (%d suppressed)",
    s.PacketsPerPoll(), s.Polls, s.Kicks, s.KicksSuppressed)
```

[`examples/drop`](examples/drop) prints `polls/s` and `pkt/poll` on its
per-second line — that is where these are easiest to see against real traffic.

## Cleanup and lifecycle

Call `fleet.Close()` (or `program.Detach`) when you are done. It removes the XDP
program from the interface and frees the BPF maps. Wire it up for both normal exit
and signals:

```go
fleet, _ := afxdp.Open("eth0", afxdp.WithUDPPorts(7000))
defer fleet.Close()

sig := make(chan os.Signal, 1)
signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
<-sig // then return, so the deferred Close runs
```

Crash safety: `Open`/`Attach` attach the program through a **BPF link**, which the
kernel auto-detaches when the process exits, even on a panic or `kill -9` (Linux
5.9+). On older kernels it falls back to the legacy netlink attach, which survives
a crash: the program stays bound to the interface, and since the sockets are gone
it drops matching traffic until removed. Recover a leftover program with
`sudo ip link set dev eth0 xdp off`. `Open` also clears any program already
attached before attaching its own, so a restart after an unclean exit just works.

## Terminology

AF_XDP has its own vocabulary; a quick glossary so the code reads clearly.

**AF_XDP** is the Linux socket family that delivers packets from a NIC driver
straight to userspace, skipping the kernel network stack. An **XSK** ("XDP
socket") is a single AF_XDP socket bound to one NIC receive queue; `xsk` is the
conventional variable name for one (from the kernel and libbpf code), and here an
XSK is the [`Socket`](socket.go) type. The **UMEM** is the region of memory shared
with the kernel that holds packet buffers, called *frames*. The **rings** are the
four single-producer/single-consumer queues between you and the kernel: *fill* and
*rx* on the receive side, *tx* and *completion* on the transmit side, and the
library drives them for you. A **Fleet** (this library's own term, not standard
AF_XDP) is a set of XSKs, one per receive queue, bound together under one XDP
program so you capture every queue at once.

## Under the hood

This is a fork of [`asavie/xdp`](https://github.com/asavie/xdp). It keeps that
project's proven UMEM and ring setup and changes two things that matter in
production.

**Independent rx/tx frame pools.** The upstream library kept a single free-frame
list shared by both directions. A receive goroutine refilling the fill ring while
a transmit goroutine sent packets could be handed the *same* UMEM frame, so a
frame got overwritten while the NIC was still DMA-ing it, corrupting packets on
the wire. The failure is silent: every local counter reads clean and you only see
it as drops at the peer (and, under WireGuard, a TCP retransmit collapse). It hits
hardest on weak-memory-model CPUs like ARM/Graviton. This fork splits the UMEM
into a disjoint receive pool and transmit pool, each owned by one direction, so
there is no shared mutable state on the data path, hence the lock-free one-rx plus
one-tx guarantee above. The ring indices are also accessed with acquire/release
atomics, as the protocol requires, so it is correct on weak-memory CPUs too. (It
also replaces an O(N) free-frame scan with an O(1) pool.)

**All queues, easily, with optional filtering.** Real NICs spread received traffic
across several rx queues (RSS); a socket bound to queue 0 sees only its slice.
`Open` binds one socket to every queue (or a subset with `WithQueues`) under a
single XDP program, and `WithFilter` controls which packets that program redirects
versus passes to the kernel, without you hand-writing per-queue maps or eBPF.

If you need the low-level pieces, `NewProgram`, `NewSocket`, and `Program.Attach`
/ `Register` are exported too; `Open` is just the convenient assembly of them.

## Requirements

AF_XDP needs `CAP_NET_RAW` (or root) and enough locked memory for the BPF maps and
UMEM (raise `RLIMIT_MEMLOCK`, e.g. `ulimit -l`). `Open` picks native zero copy
when the driver supports it and otherwise falls back automatically, so you do not
have to, and `Fleet.Info()` shows what you got. On AWS ENA, zero copy additionally
needs page-sized frames (Open defaults `FrameSize` to 4096 there automatically)
and a non-jumbo MTU, since the driver caps XDP MTU at 3502 (`ip link set ens5 mtu
1500`). Driver versions before 2.17.0 also need halved channels. See the AWS EC2 /
ENA section.

## Credits and license

Forked from [`asavie/xdp`](https://github.com/asavie/xdp) (BSD-3-Clause); the
UMEM/ring mmap and bind logic and the embedded XDP redirect program derive from
that project. The descriptor-pool, concurrency, multi-queue, and filter layers are
new work here. BSD-3-Clause, see [LICENSE](LICENSE).
