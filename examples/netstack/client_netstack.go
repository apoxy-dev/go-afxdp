//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	afxdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"github.com/vishvananda/netlink"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The load generator on netstack (-client ... -stack netstack).
//
// A kernel client is the ceiling well before 100G: its TCP costs the peer
// box what ours costs us, and at a million requests a second it is the
// bigger share. This client runs the same stack, the same way the run-to-
// completion server does: no goroutine per connection, a waiter callback
// that marks a connection ready, and the receive goroutine that delivered
// the batch servicing every ready connection right after it.
//
// It uses one stack over all queues rather than one per queue: replies land
// on whichever queue RSS hashes the connection's four-tuple to, and a client
// picks its source port before it knows that — choosing ports so RSS lands
// each flow on its own queue would need the NIC's Toeplitz key. With a
// single stack any queue's goroutine can service any connection.

// clientXDPOptions is extra go-afxdp options for the client's fleet. The
// veth test asks for generic XDP with it, which a virtual device needs.
var clientXDPOptions []afxdp.Option

// clientTestRSS, when set, stands in for the NIC's RSS table (a veth has
// none), and clientPortLo/Hi bound the source ports the file workload
// cycles through; tests shrink the range so the cycle wraps quickly.
var (
	clientTestRSS              *rss
	clientPortLo, clientPortHi = 2048, 65500
)

type clientSet struct {
	cfg      clientConfig
	st       *clientStats
	units    []*rtcServer // one per stack, in queue order
	stacks   []*stack.Stack
	conns    []*clientConn
	stopping atomic.Bool
	block    []byte // the bulk payload, sent over and over
	// The file workload (client_files.go): the objects, its own statistics,
	// and per stack a cycle of source ports whose replies RSS sends back to
	// that stack's queue.
	files  *fileSet
	fst    *filesStats
	fcfg   filesConfig
	ports  [][]uint16
	portAt []int
	portMu sync.Mutex // portAt: several receive goroutines redial in single-stack mode
	target tcpip.FullAddress
	local  tcpip.Address
}

type clientConn struct {
	cs        *clientSet
	q         *rtcServer // the ready list of the stack this connection lives on
	ep        tcpip.Endpoint
	wq        *waiter.Queue
	entry     waiter.Entry
	queued    atomic.Bool
	urgent    atomic.Bool
	mu        sync.Mutex
	connected bool
	closed    bool
	req       []byte
	got       int       // response bytes received for the request in flight
	sent      time.Time // when the request in flight went out
	qi        int       // which stack this connection lives on
	epClosed  bool      // ep has been closed and not yet replaced (see closeEndpoint)
	fst       *filesStats
	fl        filesState // the file workload's state (client_files.go)
}

// runNetstackFilesClient drives the file workload from the netstack client;
// it shares everything but the per-connection state machine with the
// request/response and bulk roles (client_files.go).
func runNetstackFilesClient(fcfg filesConfig, iface string, vlan uint16, prefix netip.Prefix, mtu uint32,
	tm xdplink.TxMode, gro, gso bool) *filesStats {
	f, err := newFileSet(fcfg.mix)
	if err != nil {
		log.Fatalf("-mix: %v", err)
	}
	if fcfg.workers <= 0 {
		log.Fatalf("-workers must be positive")
	}
	return runNetstackClient(clientConfig{target: fcfg.target, conns: fcfg.workers, msg: 1,
		duration: fcfg.duration, files: f, filesCfg: &fcfg}, iface, vlan, prefix, mtu, tm, gro, gso)
}

// runNetstackClient returns the file workload's statistics when it ran one,
// so a test can check them; the request/response and bulk roles report
// through the log as before.
func runNetstackClient(cfg clientConfig, iface string, vlan uint16, prefix netip.Prefix, mtu uint32,
	tm xdplink.TxMode, gro, gso bool) *filesStats {
	if cfg.conns <= 0 || cfg.msg <= 0 {
		log.Fatalf("-conns and -msg must be positive")
	}
	if cfg.reconnect {
		log.Fatalf("-reconnect is not supported with -stack netstack")
	}
	host, portStr, err := net.SplitHostPort(cfg.target)
	if err != nil {
		log.Fatalf("-client %q: %v", cfg.target, err)
	}
	targetIP, err := netip.ParseAddr(host)
	if err != nil || !targetIP.Is4() {
		log.Fatalf("-client %q: want an IPv4 address", cfg.target)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		if strings.Contains(portStr, "-") || strings.Contains(cfg.target, ",") {
			log.Fatalf("-client %q: the netstack client dials one port; port ranges and lists are the kernel client's", cfg.target)
		}
		log.Fatalf("-client %q: bad port", cfg.target)
	}
	port := uint16(p)

	var st clientStats
	cs := &clientSet{cfg: cfg, st: &st}
	if cfg.files != nil {
		cs.files, cs.fcfg = cfg.files, *cfg.filesCfg
		cs.fst = newFilesStats(cs.files)
	}
	if cfg.bulk {
		cs.block = make([]byte, 64<<10)
		fill(cs.block)
	}
	// One stack per queue when the NIC's RSS is readable: each stack's
	// connections use source ports whose replies hash to its own queue (see
	// rss.go). Otherwise one stack over all queues, which any queue's
	// goroutine can service, at the cost of sharing it.
	hashing, err := readRSS(iface)
	if clientTestRSS != nil {
		hashing, err = clientTestRSS, nil
	}
	if err != nil {
		log.Printf("client: %v; using one stack over all queues", err)
	}
	// clientStack tells startNetstack to build a load generator's stack
	// rather than a server's; put it back so a later caller in the same
	// process (the tests) gets a server.
	clientStack = true
	defer func() { clientStack = false }()
	rt, err := startNetstack(iface, vlan, prefix, port, mtu, tm, gro, gso, hashing != nil, func() *rtcServer {
		r := newRTCServer(nil)
		r.client = true
		cs.units = append(cs.units, r)
		return r
	}, clientXDPOptions...)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer rt.stop()
	if hashing != nil {
		log.Printf("client: %d stacks, one per queue; source ports chosen by RSS (%d-entry table)", len(rt.units), len(hashing.indir))
	}

	// The stack answers no ARP and asks none; its first SYN needs the
	// server's MAC, and the kernel, which owns the address, knows it.
	var mac [6]byte
	if peerMACFlag != "" {
		hw, err := net.ParseMAC(peerMACFlag)
		if err != nil || len(hw) != 6 {
			log.Fatalf("-peer-mac %q: want a 48-bit MAC", peerMACFlag)
		}
		copy(mac[:], hw)
	} else if mac, err = kernelNeighbour(targetIP); err != nil {
		log.Fatalf("%v", err)
	}
	for _, u := range rt.units { // every stack sends its own first SYN
		u.ep.AddNeighbour(targetIP.As4(), mac)
	}
	log.Printf("client: %s via netstack on %s, %d connections, %s", cfg.target, iface, cfg.conns, describe(cfg))

	target := tcpip.FullAddress{Addr: tcpip.AddrFrom4(targetIP.As4()), Port: port}
	local := tcpip.AddrFrom4(prefix.Addr().As4())
	cs.target, cs.local = target, local
	for _, u := range rt.units {
		cs.stacks = append(cs.stacks, u.s)
	}
	cs.ports, cs.portAt = make([][]uint16, len(rt.units)), make([]int, len(rt.units))
	// The first dials are spread over -ramp, as the kernel client's are: one
	// burst of every connection's SYN and request is a load no real server
	// sees, and it was the whole run's tail latency.
	var ramp time.Duration
	if cs.files != nil && cfg.conns > 0 {
		ramp = cs.fcfg.ramp / time.Duration(cfg.conns)
	}
	pace := func() {
		if ramp > 0 {
			time.Sleep(ramp)
		}
	}
	if hashing == nil {
		for i := 0; i < cfg.conns; i++ {
			pace()
			c, err := cs.connect(rt.units[0].s, cs.units[0], target, tcpip.FullAddress{})
			if err != nil {
				st.fail("connect", err)
				continue
			}
			cs.conns = append(cs.conns, c)
		}
	} else {
		per := (cfg.conns + len(rt.units) - 1) / len(rt.units)
		made := 0
		for q := range rt.units {
			want := min(per, cfg.conns-made)
			// The file workload closes and dials again, so it needs more
			// ports than connections: a cycle several times as long, since
			// a port it comes back to may still be in TIME-WAIT.
			// The file workload closes and dials again, so it wants every
			// port this queue can have, not just one per connection: at
			// 30k connections a second a queue dials several hundred times
			// a second, and a port it comes back to must be out of
			// TIME-WAIT (200 ms here). The others need one port each.
			lo, hi, ask := 10000, 60000, want
			if cs.files != nil {
				lo, hi, ask = clientPortLo, clientPortHi, 1<<30
			}
			ports := hashing.portsForQueue(prefix.Addr(), targetIP, port, q, lo, hi, ask)
			if cs.files != nil {
				log.Printf("client: queue %d: %d source ports for %d connections", q, len(ports), want)
			}
			cs.ports[q] = ports
			if len(ports) > want {
				ports = ports[:want]
				cs.portAt[q] = want // the redials carry on past the ports in use
			}
			if len(ports) < want {
				log.Printf("client: queue %d: only %d source ports map to it, wanted %d", q, len(ports), want)
			}
			for _, p := range ports {
				pace()
				c, err := cs.connect(rt.units[q].s, cs.units[q], target, tcpip.FullAddress{Addr: local, Port: p})
				if err != nil {
					st.fail("connect", err)
					continue
				}
				c.qi = q
				cs.conns = append(cs.conns, c)
				made++
			}
		}
	}

	done := make(chan struct{})
	time.AfterFunc(cfg.duration, func() { cs.stopping.Store(true); close(done) })
	go cs.senderReport(done, rt)
	if cs.files != nil {
		filesReport(cs.fcfg, cs.fst, cs.files, done)
	} else {
		reportLoop(cfg, &st, done)
	}
	es, ts := rt.totals()
	log.Printf("ep: rx %d tx %d txDeferred %d txDropped %d unknownMAC %d badCsum %d wrongVLAN %d notIPv4 %d | tcp: est %d segIn %d retx %d csumErr %d",
		es.Rx, es.Tx, es.TxDeferred, es.TxDropped, es.UnknownMAC, es.RxBadCsum, es.WrongVLAN, es.NotIPv4,
		ts.established, ts.segIn, ts.retx, ts.csumErr)
	log.Printf("tcp: timeouts %d fastRetx %d slowRetx %d dsack %d spuriousRTO %d invalid %d resetsRcvd %d",
		ts.timeouts, ts.fastRetx, ts.slowRetx, ts.dsack, ts.spuriousRTO, ts.invalid, ts.resetsRcvd)
	// A few connections' TCP view, for the question "what bounds a flow".
	for i, c := range cs.conns {
		if i >= 3 {
			break
		}
		var info tcpip.TCPInfoOption
		c.mu.Lock()
		terr := c.ep.GetSockOpt(&info)
		c.mu.Unlock()
		if terr == nil {
			log.Printf("conn %d: rtt %v rttvar %v rto %v cwnd %d segs ssthresh %d cc %v", i,
				info.RTT, info.RTTVar, info.RTO, info.SndCwnd, info.SndSsthresh, info.CcState)
		}
	}
	if fs, err := rt.fleet.Stats(); err == nil {
		var polls, kicks, suppressed uint64
		for _, q := range fs.PerQueue {
			polls += q.Polls
			kicks += q.Kicks
			suppressed += q.KicksSuppressed
		}
		log.Printf("syscalls: polls %d  tx kicks %d (suppressed %d)  over the run", polls, kicks, suppressed)
	}
	for _, c := range cs.conns {
		c.mu.Lock()
		c.close()
		c.mu.Unlock()
	}
	return cs.fst
}

// connect starts one connection; the handshake completes in the background
// and the first writable event begins the exchange.
func (cs *clientSet) connect(s *stack.Stack, q *rtcServer, target, bind tcpip.FullAddress) (*clientConn, error) {
	wq := &waiter.Queue{}
	ep, terr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	ep.SocketOptions().SetDelayOption(false)
	if cs.files != nil && cs.fcfg.slow > 0 {
		ep.SocketOptions().SetReceiveBufferSize(64<<10, true) // half of being slow: a small window
	}
	if bind.Port != 0 {
		if terr := ep.Bind(bind); terr != nil {
			ep.Close()
			return nil, fmt.Errorf("bind port %d: %s", bind.Port, terr)
		}
	}
	c := &clientConn{cs: cs, q: q, ep: ep, wq: wq}
	switch {
	case cs.files != nil:
		c.fst = cs.fst
		c.fl.rnd = rand.New(rand.NewSource(int64(bind.Port)*2654435761 + 1))
		c.fl.dialPort = bind.Port
		c.req = make([]byte, 0, 64)
	case !cs.cfg.bulk:
		c.req = make([]byte, cs.cfg.msg)
		fill(c.req)
	}
	c.entry = waiter.NewFunctionEntry(waiter.ReadableEvents|waiter.WritableEvents|waiter.EventHUp|waiter.EventErr, c.notify)
	wq.EventRegister(&c.entry)
	if terr := ep.Connect(target); terr != nil {
		if _, ok := terr.(*tcpip.ErrConnectStarted); !ok {
			wq.EventUnregister(&c.entry)
			ep.Close()
			return nil, errors.New(terr.String())
		}
	}
	return c, nil
}

// notify is the waiter callback: fired under the endpoint's lock, so it only
// marks the connection ready.
func (c *clientConn) notify(m waiter.EventMask) {
	readable := m&waiter.ReadableEvents != 0
	if c.queued.CompareAndSwap(false, true) {
		c.q.enqueue(c, readable)
		return
	}
	if readable && c.urgent.CompareAndSwap(false, true) {
		c.q.enqueue(c, true) // see rtcConn.notify
	}
}

func (c *clientConn) unqueue() { c.queued.Store(false); c.urgent.Store(false) }

// service runs on the receive goroutine after a batch: finish the handshake
// once, then read what has arrived and send what is due.
func (c *clientConn) service(buf []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.cs.stopping.Load() {
		c.close()
		return
	}
	if c.cs.files != nil {
		c.serviceFiles(buf)
		return
	}
	st := c.cs.st
	if !c.connected {
		if tcp.EndpointState(c.ep.State()) != tcp.StateEstablished {
			if terr := c.ep.LastError(); terr != nil {
				st.fail("connect", errors.New(terr.String()))
				c.close()
			}
			return // still connecting; the next event says more
		}
		c.connected = true
		st.conns.Add(1)
		if c.cs.cfg.bulk {
			if !c.cs.cfg.recvOnly {
				c.sendBulk()
			}
		} else {
			c.sendRequest()
		}
	}
	// Read everything that has arrived.
	for {
		w := tcpip.SliceWriter(buf)
		res, terr := c.ep.Read(&w, tcpip.ReadOptions{})
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
				break
			}
			if !c.cs.stopping.Load() {
				st.fail("read", errors.New(terr.String()))
			}
			c.close()
			return
		}
		if c.cs.cfg.bulk {
			st.bytes.Add(uint64(res.Count))
			continue
		}
		c.got += res.Count
		for c.got >= len(c.req) {
			c.got -= len(c.req)
			st.reqs.Add(1)
			st.bytes.Add(uint64(len(c.req)))
			st.hist.record(time.Since(c.sent))
			c.sendRequest()
		}
	}
	if c.cs.cfg.bulk && !c.cs.cfg.recvOnly {
		c.sendBulk() // the writable event brought us here, or reads made room
	}
}

func (c *clientConn) sendRequest() {
	c.sent = time.Now()
	if _, terr := c.ep.Write(bytes.NewReader(c.req), tcpip.WriteOptions{}); terr != nil {
		if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
			c.cs.st.fail("write", errors.New(terr.String()))
			c.close()
		}
	}
}

// sendBulk keeps the send buffer full; a full buffer returns WouldBlock and
// the writable event brings us back.
func (c *clientConn) sendBulk() {
	for {
		n, terr := c.ep.Write(bytes.NewReader(c.cs.block), tcpip.WriteOptions{})
		if c.cs.cfg.sinkOnly {
			c.cs.st.bytes.Add(uint64(n))
		}
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
				c.cs.st.fail("write", errors.New(terr.String()))
				c.close()
			}
			return
		}
		if int(n) < len(c.cs.block) {
			return
		}
	}
}

// closeEndpoint drops this connection's endpoint but keeps the clientConn,
// so a file-workload connection can be dialled again in place (redial).
// epClosed remembers that, so a stop that lands while the connection waits
// to dial again does not close the same endpoint twice.
func (c *clientConn) closeEndpoint() {
	c.wq.EventUnregister(&c.entry)
	c.ep.Close()
	c.connected, c.epClosed = false, true
}

func (c *clientConn) close() {
	if c.closed {
		return
	}
	c.closed = true
	if !c.epClosed {
		c.closeEndpoint()
	}
}

// kernelNeighbour returns the MAC the kernel has for ip, provoking an ARP
// exchange with a UDP datagram if it has none yet.
func kernelNeighbour(ip netip.Addr) ([6]byte, error) {
	const usable = netlink.NUD_REACHABLE | netlink.NUD_STALE | netlink.NUD_PERMANENT | netlink.NUD_DELAY | netlink.NUD_PROBE
	for attempt := 0; attempt < 30; attempt++ {
		neighs, err := netlink.NeighList(0, netlink.FAMILY_V4)
		if err != nil {
			return [6]byte{}, fmt.Errorf("neighbour table: %v", err)
		}
		for _, n := range neighs {
			if n.IP.Equal(ip.AsSlice()) && len(n.HardwareAddr) == 6 && n.State&usable != 0 {
				return [6]byte(n.HardwareAddr), nil
			}
		}
		if u, err := net.Dial("udp", net.JoinHostPort(ip.String(), "9")); err == nil {
			_, _ = u.Write([]byte{0})
			u.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return [6]byte{}, fmt.Errorf("no ARP entry for %s (is it on this link?)", ip)
}

// senderReport prints, once a second, what bounds the sender: the first
// connections' congestion window and round trip, and the stack's loss
// counters. A window that grows and collapses is loss; a flat window with a
// flat round trip is neither, and the answer is elsewhere.
func (cs *clientSet) senderReport(done <-chan struct{}, rt *netstackRuntime) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last tcpTotals
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			var line string
			for i, c := range cs.conns {
				if i >= 2 {
					break
				}
				var info tcpip.TCPInfoOption
				c.mu.Lock()
				terr := c.ep.GetSockOpt(&info)
				c.mu.Unlock()
				if terr == nil {
					line += fmt.Sprintf("  [%d cwnd %d ssthresh %d rtt %v]", i, info.SndCwnd, info.SndSsthresh, info.RTT.Round(10*time.Microsecond))
				}
			}
			_, ts := rt.totals()
			log.Printf("sender:%s | retx %d (fast %d slow %d timeouts %d) dsack %d spuriousRTO %d",
				line, ts.retx-last.retx, ts.fastRetx-last.fastRetx, ts.slowRetx-last.slowRetx,
				ts.timeouts-last.timeouts, ts.dsack-last.dsack, ts.spuriousRTO-last.spuriousRTO)
			last = ts
		}
	}
}
