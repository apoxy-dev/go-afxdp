//go:build linux

// natlb is a small NAT-mode TCP load balancer on AF_XDP.
//
// Clients connect to a VIP; natlb picks a backend, rewrites the packet so it
// is addressed to that backend and sourced from the load balancer, and sends
// it back out the same interface. Replies are rewritten the other way, so the
// client believes it is talking to the VIP and the backend believes it is
// talking to the load balancer. Nothing terminates TCP: this only translates
// addresses and ports.
//
//	go build -o natlb ./examples/natlb
//	sudo ./natlb -iface eth0 \
//	     -vip 203.0.113.10:443 \
//	     -nat-ip 10.0.0.10 \
//	     -backend 10.0.0.21:443@02:00:00:00:00:21 \
//	     -backend 10.0.0.22:443@02:00:00:00:00:22
//
// This is a proof of concept for the datapath, not a load balancer you should
// run. See the README for what it deliberately does not do.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atoonk/go-afxdp"
)

// counters are the datapath's own tallies. The flow table keeps its separate.
type counters struct {
	rx, forward, reverse    atomic.Uint64
	noState, notOurs, parse atomic.Uint64
	txFull                  atomic.Uint64
}

func main() {
	iface := flag.String("iface", "eth0", "interface to bind")
	vipFlag := flag.String("vip", "", "virtual address clients connect to, ip:port (required)")
	natFlag := flag.String("nat-ip", "", "address backends see traffic coming from (required)")
	var backends multiFlag
	flag.Var(&backends, "backend", "backend as ip:port@mac, repeatable (required)")
	portLo := flag.Int("nat-port-lo", 20000, "first NAT source port")
	portHi := flag.Int("nat-port-hi", 29999, "last NAT source port")
	idle := flag.Duration("idle-timeout", 2*time.Minute, "drop a flow after this long with no packets")
	drain := flag.Duration("drain-timeout", 5*time.Second, "drop a closing flow this long after both FINs")
	queues := flag.Int("queues", 0, "rx queues to bind (0 = all, which is what you want: RSS spreads flows across every queue)")
	flag.Parse()

	vip, err := netip.ParseAddrPort(*vipFlag)
	if err != nil || !vip.Addr().Is4() {
		log.Fatalf("-vip %q: need an IPv4 address:port", *vipFlag)
	}
	if vip.Port() != 80 && vip.Port() != 443 {
		// The filter below redirects TCP ports 80 and 443. A VIP on any other
		// port would never reach us, which is a confusing way to see nothing
		// happen, so say so now.
		log.Fatalf("-vip port %d: this example filters TCP 80 and 443 only", vip.Port())
	}
	natIP, err := netip.ParseAddr(*natFlag)
	if err != nil || !natIP.Is4() {
		log.Fatalf("-nat-ip %q: need an IPv4 address", *natFlag)
	}
	if *portLo < 1 || *portHi > 65535 || *portLo > *portHi {
		log.Fatalf("-nat-port-lo/-nat-port-hi: %d-%d is not a usable range", *portLo, *portHi)
	}
	if len(backends) == 0 {
		log.Fatal("at least one -backend is required")
	}
	// A zero idle timeout expires every flow on the first GC tick, which looks
	// exactly like the load balancer dropping every connection for no reason.
	if *idle <= 0 || *drain <= 0 {
		log.Fatalf("-idle-timeout (%s) and -drain-timeout (%s) must both be positive", *idle, *drain)
	}
	if *drain > *idle {
		log.Fatalf("-drain-timeout (%s) is longer than -idle-timeout (%s): a closing flow would "+
			"outlive an idle one, which is backwards", *drain, *idle)
	}
	// The NAT range must not overlap what the kernel hands out to this host's
	// own sockets, or a connection natlb originates can collide with a flow it
	// is translating.
	if lo, hi, err := ephemeralRange(); err == nil && *portLo <= hi && lo <= *portHi {
		log.Printf("warning: NAT ports %d-%d overlap this host's ephemeral range %d-%d; "+
			"pick a range below it (the default 20000-29999 is)", *portLo, *portHi, lo, hi)
	}

	ifi, err := net.InterfaceByName(*iface)
	if err != nil || len(ifi.HardwareAddr) != 6 {
		log.Fatalf("no Ethernet address on %s: %v", *iface, err)
	}
	var lbMAC [6]byte
	copy(lbMAC[:], ifi.HardwareAddr)

	table := NewTable(natIP, uint16(*portLo), uint16(*portHi))
	for _, s := range backends {
		addr, mac, err := parseBackend(s)
		if err != nil {
			log.Fatalf("-backend %q: %v", s, err)
		}
		if addr.Port() != 80 && addr.Port() != 443 {
			// The reply would carry source port 8080, which matches neither
			// half of the filter below, so it would never come back to us:
			// every connection would hang half-open. Refuse it here rather
			// than at packet time.
			log.Fatalf("-backend %s: this example filters TCP 80 and 443 only, so replies "+
				"from port %d would never reach it", s, addr.Port())
		}
		table.AddBackend(addr, mac)
		log.Printf("backend %s via %s", addr, net.HardwareAddr(mac[:]))
	}

	// Both directions have to reach us, and XDP only sees ingress:
	//
	//	client  -> VIP        destination port 80/443
	//	backend -> NAT-IP     source port 80/443
	//
	// WithFilter ORs its matches, and there is no AND builder, so this is
	// wider than the service: any TCP with port 80 or 443 on either side is
	// redirected, not just traffic for our VIP. That is fine on a test rig and
	// is the first thing to tighten for real use -- see the README.
	// WithKeepManagement spares this box's own SSH either way.
	opts := []afxdp.Option{
		afxdp.WithFilter(
			afxdp.MatchTCPPort(80, 443),
			afxdp.MatchTCPSrcPort(80, 443),
		),
		afxdp.WithKeepManagement(),
		afxdp.WithQueues(*queues),
		// The received frame is rewritten in place and transmitted as it is;
		// Complete routes it back to the receive pool it came from.
		afxdp.WithTxReuseRxFrames(),
	}
	fleet, err := afxdp.Open(*iface, opts...)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer fleet.Close()
	if info, err := fleet.Info(); err == nil {
		log.Printf("%s", info)
	}
	log.Printf("vip %s -> %d backend(s), NAT %s:%d-%d", vip, len(backends), natIP, *portLo, *portHi)

	log.Printf("waiting for the link (a native XDP attach bounces it)...")
	if !fleet.WaitLinkUp(15 * time.Second) {
		log.Printf("warning: %s not up after 15s; running anyway", *iface)
	}

	var c counters
	lb := &balancer{table: table, vip: vip, natIP: natIP, lbMAC: lbMAC, c: &c,
		portLo: uint16(*portLo), portHi: uint16(*portHi)}
	for _, xsk := range fleet.Sockets() {
		go lb.run(xsk)
	}
	go gc(table, *idle, *drain)
	go report(table, &c)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("stopped")
}

// balancer is the per-packet logic, shared by every queue's goroutine. Its own
// fields are read-only after startup; the flow table it points at is the only
// mutable state, and that is guarded by the table's own lock.
type balancer struct {
	table          *Table
	vip            netip.AddrPort
	natIP          netip.Addr
	lbMAC          [6]byte
	portLo, portHi uint16
	c              *counters
}

// run is one queue's loop. It follows examples/fwd: reclaim, refill, poll with
// a bound, translate what we can and recycle the rest.
func (b *balancer) run(xsk *afxdp.Socket) {
	out := make([]afxdp.Desc, 0, 256)
	drop := make([]afxdp.Desc, 0, 256)
	for {
		xsk.Complete(xsk.NumCompleted())
		xsk.Fill(xsk.NumFreeFillSlots())
		// Bounded, like fwd's: with TxReuseRxFrames every receive frame can be
		// in flight on transmit at once, and a forever poll would then wait for
		// packets that cannot arrive while the completions that would free the
		// frames sit unread above.
		n, err := xsk.Poll(100 * time.Millisecond)
		if err != nil {
			return // closed
		}
		rx := xsk.Receive(n)
		if len(rx) == 0 {
			continue
		}
		b.c.rx.Add(uint64(len(rx)))
		out, drop = out[:0], drop[:0]
		now := time.Now()
		for _, d := range rx {
			if b.translate(xsk.GetFrame(d), now) {
				out = append(out, d)
			} else {
				drop = append(drop, d)
			}
		}
		if len(out) > 0 {
			sent := xsk.Transmit(out)
			if sent < len(out) {
				// A full transmit ring is a drop, not something to spin on.
				b.c.txFull.Add(uint64(len(out) - sent))
				drop = append(drop, out[sent:]...)
			}
		}
		xsk.Recycle(drop)
	}
}

// translate rewrites one frame in place and reports whether it should go out.
func (b *balancer) translate(f []byte, now time.Time) bool {
	p, perr := parse(f)
	if perr != parseOK {
		// "Not IPv4/TCP" is ordinary here: the filter is wider than the
		// service. Only a malformed or fragmented packet is worth counting as
		// an error.
		if perr == parseTooShort || perr == parseFragment {
			b.c.parse.Add(1)
		} else {
			b.c.notOurs.Add(1)
		}
		return false
	}

	switch {
	// Client -> VIP: pick a backend, translate both ends.
	case p.dst == b.vip.Addr() && p.dport == b.vip.Port():
		var clientMAC [6]byte
		copy(clientMAC[:], f[6:12]) // whoever handed it to us is where replies go
		fl, ok := b.table.Forward(p, clientMAC, now)
		if !ok {
			b.c.noState.Add(1)
			return false
		}
		rewrite(f, p, b.lbMAC, fl.backend.MAC,
			b.natIP, fl.backend.Addr.Addr(),
			fl.natPort, fl.backend.Addr.Port())
		b.c.forward.Add(1)
		return true

	// Backend -> us on a NAT port: put the VIP and the client back.
	case p.dst == b.natIP && p.dport >= b.portLo && p.dport <= b.portHi:
		fl, ok := b.table.Reverse(p, now)
		if !ok {
			b.c.noState.Add(1)
			return false
		}
		rewrite(f, p, b.lbMAC, fl.clientMAC,
			fl.fwd.dst, fl.fwd.src, // the VIP and the client, as they arrived
			fl.fwd.dport, fl.fwd.sport)
		b.c.reverse.Add(1)
		return true
	}

	b.c.notOurs.Add(1)
	return false
}

// gc walks the table off the packet path and drops what has gone quiet.
func gc(t *Table, idle, drain time.Duration) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for now := range tick.C {
		t.Expire(now, idle, drain)
	}
}

// report logs once a second. Never per packet: a dataplane that logs per
// packet is a dataplane that measures its own logging.
func report(t *Table, c *counters) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last struct{ rx, fwd, rev uint64 }
	for range tick.C {
		rx, fw, rv := c.rx.Load(), c.forward.Load(), c.reverse.Load()
		st := t.Stats()

		var per []string
		for _, be := range t.Backends() {
			s := fmt.Sprintf("%s=%d", be.Addr, be.Active)
			if be.Drained {
				s += "(draining)"
			}
			per = append(per, s)
		}
		// Rates per second on the left, totals since start in brackets.
		log.Printf("rx %d/s  fwd %d/s  rev %d/s | flows %d (total: new %d, gone %d, reset %d) | ports %d/%d free | %s",
			rx-last.rx, fw-last.fwd, rv-last.rev,
			st.Active, st.Created, st.Expired, st.Reset,
			st.FreePorts, st.TotalPorts, strings.Join(per, " "))

		// Why packets did not go out. not-ours is usually the big one: the
		// filter is deliberately wider than the service, so traffic on 80/443
		// that is nothing to do with the VIP still reaches us and is dropped.
		// Printed whenever anything was dropped at all, so rx always accounts
		// for itself: rx == fwd + rev + these.
		no, nt, ml, tf := c.noState.Load(), c.notOurs.Load(), c.parse.Load(), c.txFull.Load()
		if no|nt|ml|tf|st.NoPort != 0 {
			log.Printf("  dropped (total): not-ours %d  no-state %d  malformed %d  port-exhausted %d  tx-full %d",
				nt, no, ml, st.NoPort, tf)
		}
		last.rx, last.fwd, last.rev = rx, fw, rv
	}
}

// ephemeralRange reads the port range the kernel hands to this host's own
// outbound sockets, so a NAT range overlapping it can be pointed out at
// startup rather than discovered as an occasional collision.
func ephemeralRange() (lo, hi int, err error) {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, 0, err
	}
	if _, err := fmt.Sscan(string(b), &lo, &hi); err != nil {
		return 0, 0, err
	}
	return lo, hi, nil
}

// parseBackend reads "10.0.0.21:443@02:00:00:00:00:21".
//
// The MAC is required because this example does no ARP: it has to put a real
// next hop in the Ethernet header, and asking the operator is the smallest
// honest way to get one.
func parseBackend(s string) (netip.AddrPort, [6]byte, error) {
	var mac [6]byte
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return netip.AddrPort{}, mac, fmt.Errorf("want ip:port@mac")
	}
	addr, err := netip.ParseAddrPort(s[:at])
	if err != nil {
		return netip.AddrPort{}, mac, fmt.Errorf("address: %w", err)
	}
	if !addr.Addr().Is4() {
		return netip.AddrPort{}, mac, fmt.Errorf("IPv4 only")
	}
	hw, err := net.ParseMAC(s[at+1:])
	if err != nil || len(hw) != 6 {
		return netip.AddrPort{}, mac, fmt.Errorf("bad MAC %q", s[at+1:])
	}
	copy(mac[:], hw)
	return addr, mac, nil
}

// multiFlag collects a repeated string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }
