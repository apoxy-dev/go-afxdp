# natlb: a NAT-mode TCP load balancer on AF_XDP

A client opens a connection to a virtual address. `natlb` picks a backend,
rewrites the packet so it is addressed to that backend and sourced from the
load balancer, and sends it back out the interface it arrived on. The reply is
rewritten the other way. The client thinks it is talking to the VIP; the
backend thinks it is talking to the load balancer; neither is right, and
neither can tell.

Nothing here terminates TCP. There is no socket, no handshake, no buffering.
`natlb` only edits addresses and ports in packets that pass through it, which
is what makes it a load balancer you can read in an afternoon.

This is a proof of concept for the datapath. Read the limitations before you
get ideas.

## What NAT mode is

```
        1.2.3.4:5678                                  10.0.0.21:443
             |                                              ^
             |  dst 203.0.113.10:443                        |
             v                                              |
    +--------------------+   src 10.0.0.10:40001   +--------+--------+
    |       natlb        |------------------------>|    backend 1    |
    |  VIP 203.0.113.10  |   dst 10.0.0.21:443     +-----------------+
    |  NAT 10.0.0.10     |
    +--------------------+   src 10.0.0.10:40002   +-----------------+
             ^  |            dst 10.0.0.22:443     |    backend 2    |
             |  |          ------------------------>|  10.0.0.22:443 |
             |  v                                   +-----------------+
        replies, rewritten
        back to the VIP
```

Forward, per packet:

| field       | before             | after            |
|-------------|--------------------|------------------|
| Ethernet    | client → LB        | LB → backend     |
| IPv4 source | `1.2.3.4`          | `10.0.0.10`      |
| IPv4 dest   | `203.0.113.10`     | `10.0.0.21`      |
| TCP source  | `5678`             | `40001`          |
| TCP dest    | `443`              | `443`            |

Reverse, exactly undone:

| field       | before             | after            |
|-------------|--------------------|------------------|
| Ethernet    | backend → LB       | LB → client      |
| IPv4 source | `10.0.0.21`        | `203.0.113.10`   |
| IPv4 dest   | `10.0.0.10`        | `1.2.3.4`        |
| TCP source  | `443`              | `443`            |
| TCP dest    | `40001`            | `5678`           |

Both IPv4 and TCP checksums are recomputed. The length never changes, which is
what lets the received frame be transmitted without a copy.

## Why the source port has to change

The obvious implementation keeps the client's source port and rewrites only
the addresses. It works until two clients pick the same one.

Client `1.2.3.4:5678` and client `5.6.7.8:5678` are distinct at the VIP,
because their addresses differ. After SNAT they are both `10.0.0.10:5678`, and
if they land on the same backend they are the *same connection* as far as that
backend is concerned. The second SYN looks like a retransmission of the first.

So the source port is allocated, not preserved: each flow gets its own from a
pool, which makes the backend-facing tuple unique by construction. That is the
"PAT" half of SNAT/PAT — source NAT plus port translation — and it is why
there has to be a connection table at all: the reply comes back addressed to
`10.0.0.10:40001` and nothing in that packet says who the client was. Only the
table knows.

## The connection table

Two keys per flow, one per direction, both `{proto, src, sport, dst, dport}`:

```
forward   {TCP, 1.2.3.4,   5678, 203.0.113.10, 443}   -> flow
reverse   {TCP, 10.0.0.21, 443,  10.0.0.10,    40001} -> the same flow
```

A reply is an ordinary map lookup on the reverse key. The flow holds the
backend, the NAT port, and the MAC the request came from, which is where
replies are sent.

**Affinity**: a flow is pinned to the backend chosen for its first packet and
stays there for life, even if the backend is removed from rotation. Moving an
established TCP connection to a different server does not load-balance it, it
breaks it.

**Backend selection** is the power of two choices: sample two backends at
random, take whichever carries fewer flows. Barely more code than round robin
and much better behaved when connections are unequal in length, because it
balances what is actually outstanding rather than the count of assignments.

**Backends** are `AddBackend` / `RemoveBackend`. There is no health checking:
whatever knows about health can call those. Removing a backend drains it: its
existing flows keep running and only new ones stop arriving, and it disappears
once its last flow has gone. Re-adding a drained backend revives the same entry
rather than making a second one, so it keeps its flow count and selection is
not misled.

In *this binary* nothing calls them after startup: the backend set is fixed on
the command line. They are there for code that imports this, and for whatever
you would rather have deciding health.

**Expiry** has two timeouts. A connection where both sides have sent a FIN only
has to outlive the final ACK, so it gets `-drain-timeout` (5s). Anything else
gets `-idle-timeout` (2m), because an established connection with nothing to
say is still real. An RST ends the flow at the next sweep, at most a second
later, rather than on the spot: the reset itself still has to be translated,
and it carries that flow's NAT port. Returning the port during the lookup would
let the next connection take it while the reset is in flight, and the reset
would land on that connection instead. A garbage collector runs once a second,
off the packet path, and returns NAT ports to the pool.

Ports are handed out FIFO, so a port from a just-closed flow goes to the back
of the queue rather than straight to a new connection a straggling packet could
land on.

## The datapath

```
Complete  ->  Fill  ->  Poll  ->  Receive
                                     |
                    parse  ->  look up flow  ->  rewrite in place
                                     |
                                 Transmit (the same frame)
```

`WithTxReuseRxFrames()` is what makes this zero-copy: the frame that arrived is
the frame that leaves, and `Complete` routes it back to the receive pool it came
from. Nothing is copied and nothing is allocated per packet.

The poll is bounded, for the reason `examples/fwd` explains: every receive frame
can be in flight on transmit at once, and a forever poll would wait for packets
that cannot arrive while the completions that would free the frames sit unread.

Parsing is deliberately careful in three places, all of them fields an attacker
controls. The TCP header is found through the IPv4 **IHL** field rather than
assumed to be 20 bytes in, so a packet carrying IP options is not read at the
wrong offset. The IPv4 **total length** is checked to describe a real segment
inside the frame, because it decides where the checksum stops: unchecked, a
frame could be rewritten and then quietly not re-checksummed, which puts a
corrupt packet on the wire and spends a NAT port doing it. And **fragments** are
refused outright, since only the first carries a TCP header, so translating it
would rewrite an address the rest still carry.

## Getting both directions to AF_XDP

XDP is ingress-only, so both halves of a connection have to be caught arriving:

```go
afxdp.WithFilter(
    afxdp.MatchTCPPort(80, 443),      // client -> VIP    (destination port)
    afxdp.MatchTCPSrcPort(80, 443),   // backend -> LB    (source port)
)
```

`WithFilter` ORs its matches and there is no AND builder, so **this is wider
than the service**: any TCP with port 80 or 443 on either side is redirected on
the bound interface, not just traffic for the VIP. Fine on a rig, and the first
thing to fix for real use, with `pcapfilter`:

```
(dst host 203.0.113.10 and tcp dst port 443) or
(dst host 10.0.0.10 and tcp src port 443 and dst portrange 20000-29999)
```

`WithKeepManagement()` is on, which excepts ARP, neighbour discovery, DNS and
TCP 22 — so SSH survives.

**It does not except 80 and 443, and nothing else does either.** While natlb
runs, this box loses its own HTTP and HTTPS on that interface: `apt` and `yum`,
`curl`, container image pulls, agent check-ins, HTTP metadata services, and any
local web server or management UI on 80/443. Their packets are redirected to
natlb, do not match the VIP, and are dropped. Run this on a dedicated
interface, or narrow the filter first. If you administer the box through a web
console on 443, or it is a Kubernetes node whose API server is on 443, you will
cut yourself off.

The NAT port range defaults to **20000-29999**, deliberately below the kernel's
ephemeral range (32768-60999). Anything this host originates itself therefore
cannot collide with a NAT port, and a precise filter can key on the range.

## Layer 2

The load balancer has to put a real next hop in every frame it sends, and there
is no ARP here.

- **To a backend**: the MAC is configuration, part of the `-backend` value.
- **To a client**: the source MAC of the first packet of the flow, remembered on
  the flow. Replies retrace the path the request arrived on, which is right
  whenever the client is reached through a router or is on the same segment.

The VIP can stay configured on the interface so the kernel answers ARP for it
normally. Only TCP on the filtered ports is redirected; everything else,
including ARP, goes to the stack as usual.

## Running it

```
go build -o natlb ./examples/natlb

sudo ./natlb -iface eth0 \
    -vip 203.0.113.10:443 \
    -nat-ip 10.0.0.10 \
    -backend 10.0.0.21:443@02:00:00:00:00:21 \
    -backend 10.0.0.22:443@02:00:00:00:00:22
```

| flag | meaning |
|---|---|
| `-vip ip:port` | what clients connect to. Port must be 80 or 443. |
| `-nat-ip ip` | the address backends see traffic from |
| `-backend ip:port@mac` | repeatable; MAC is the next hop. Port must also be 80 or 443, or the reply would never match the filter. |
| `-nat-port-lo/-hi` | NAT source port range, default 20000-29999 |
| `-idle-timeout` | default 2m |
| `-drain-timeout` | after both FINs, default 5s |
| `-queues` | rx queues to bind, default 0 = all. Bind them all: RSS spreads flows across every queue, so binding one silently misses most connections on a real NIC. |

Counters are logged once a second, never per packet. Rates are per second;
anything in brackets is a total since start:

```
rx 108/s  fwd 54/s  rev 54/s | flows 10 (total: new 10, gone 0, reset 0) |
  ports 9990/10000 free | 10.0.0.21:80=5 10.0.0.22:80=5
```

A second line appears whenever anything was dropped, so `rx` always accounts for
itself — `rx == fwd + rev + the drops`:

```
  dropped (total): not-ours 12  no-state 0  malformed 0  port-exhausted 0  tx-full 0
```

`not-ours` is usually the large one, and that is expected rather than alarming:
the filter is deliberately wider than the service, so traffic on 80/443 that has
nothing to do with the VIP still reaches natlb and is discarded.

## The demo

`demo.sh` builds the whole thing out of network namespaces, so you can watch it
work without a second machine or a spare NIC:

```
sudo ./examples/natlb/demo.sh up      # bridge, 3 namespaces, 2 HTTP servers, natlb
sudo ./examples/natlb/demo.sh curl    # fetch the VIP ten times from the client
sudo ./examples/natlb/demo.sh down
```

Needs root and `python3`. Everything hangs off one bridge, because `natlb`
hairpins: it receives and transmits on the same interface, so the client and
the backends have to share a segment with it. Everything it creates is named
`nlb-*` and teardown only removes that, so it will not delete an interface that
was already on your machine — but it does add `10.0.0.0/24` to the host, so
edit the script if you already use that prefix. natlb's own output goes to
`/run/natlb-demo/natlb.log`.

```
      ns nlb-cl          ns nlb-b1      ns nlb-b2
     10.0.0.1/24       10.0.0.21/24   10.0.0.22/24
          |                  |              |
          +------- nlb-br ---+--------------+
                     |
             nlb-lb0 (host)  10.0.0.10
                     |
          natlb, AF_XDP on nlb-lb0
             VIP 203.0.113.10:80
```

Real output:

```
hello from nlb-b1
hello from nlb-b2
hello from nlb-b1
...
rx 108/s  fwd 54/s  rev 54/s | flows 10 (total: new 10, gone 0, reset 0) | ports 9990/10000 free | 10.0.0.21:80=5 10.0.0.22:80=5
```

Ten HTTP requests through the AF_XDP datapath, spread across the two backends.
Selection samples two backends at random and takes the less loaded, with
replacement, so an even split is what it tends towards over many flows rather
than a guarantee for any particular ten: 6/4 is as ordinary as 5/5.

## Tests

```
go test ./examples/natlb/          # parsing, rewriting, checksums, table
sudo go test ./examples/natlb/     # adds the veth end-to-end test
```

The unit tests craft raw Ethernet/IPv4/TCP frames and check the exact tuples
that come out, with the checksums verified by summing the result the way a
receiver does rather than by repeating the calculation. The veth test runs the
real datapath over a real AF_XDP socket: a SYN goes in one end of a veth pair,
the translated packet comes out the other, the reply goes back, and the client
sees an answer from the VIP.

## Limitations

Scope, on purpose:

- IPv4 only, TCP only, VIP port 80 or 443 only.
- One VIP. One interface.
- No IP fragment support (they are counted and dropped).
- No ARP, no IPv6 ND.
- No health checking; `AddBackend`/`RemoveBackend` are the interface.

Things that would have to change before this was serious:

- **The filter is too wide.** It catches any TCP on 80/443, not just the VIP's.
- **One mutex over the whole table.** Fine for one queue; shard by flow hash or
  give each queue its own port range before scaling out.
- **A SYN flood takes it down, and it is worse than "the table grows".** The
  port pool is the cap, and reaching it is a total outage rather than
  degradation: ~10,000 spoofed SYNs (well under a millisecond at line rate) is
  enough, and because a half-open flow never sees a FIN it is held for the full
  `-idle-timeout` — two minutes by default — during which no new client can
  connect. Roughly 84 packets a second keeps it down indefinitely. Each spoofed
  SYN is also forwarded, so the backend's SYN-ACK is translated back to the
  spoofed address: natlb is a reflector sourced from the VIP. Real ones need a
  short half-open timeout, a per-source cap, and SYN cookies.
- **No state anywhere but memory.** Restarting drops every connection, and there
  is no failover: two of these do not share a table.
- **No connection draining on shutdown**, no graceful backend removal beyond
  "stop sending new flows".
- **TCP state is three flags deep.** It does not track sequence numbers, so it
  cannot tell a legitimate RST from a forged one.
- The kernel never sees these packets, so `conntrack`, `nft` and friends know
  nothing about any of it. That is the point, and it means the usual tools will
  not help you debug it.
