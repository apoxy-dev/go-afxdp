//go:build linux

package main

import (
	"bytes"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Run-to-completion echo (-mode echo-rtc).
//
// The ordinary echo is a goroutine per connection behind a net.Conn: when
// data arrives the stack wakes that goroutine, it reads through the adapter,
// writes back, and sleeps. That is one wakeup and two copies through the
// adapter per request, and the wakeup is a large share of a request's round
// trip on a LAN.
//
// This server has no goroutine per connection. It talks to the raw
// tcpip.Endpoint and registers a waiter callback that only marks the
// connection ready — it must not touch the endpoint, because netstack fires
// it while holding the endpoint's lock. The receive goroutine that delivered
// the segment (and, with the fork's inline processing, ran TCP on it) then
// services every ready connection right after its batch, on the same
// goroutine: read, write back, next. A request is handled by the thread that
// received it, the way a kernel handles it in softirq, and nobody is woken.
//
// Each stack has its own rtcServer and its own receive goroutine, so a
// connection is serviced only by the goroutine of the stack it lives on.
// The ready list still takes a lock: the callback also fires from that
// stack's processor goroutine (the inline fast path's fallback), from timer
// goroutines, and from the accept goroutine. Those are rare; the lock is
// uncontended in the steady state, and the loop's 1 ms poll timeout bounds
// how long such an off-loop notification waits.

type rtcServer struct {
	srv   *server
	mu    sync.Mutex
	ready []readyConn // connections with room to send more: the normal lane
	spare []readyConn
	// first is the lane for connections that have something to read — a
	// request, or the peer's close — and it is drained before ready. A new
	// connection's request otherwise waits behind every transfer in
	// progress that became writable, hundreds of them under load, and with
	// the pass bounded that was most of the first byte's 16 ms. Readable
	// events are rare and small next to writable ones; letting them go
	// first is what a fair queue does for a new flow.
	first      []readyConn
	spareFirst []readyConn
	bufs       sync.Pool
	lns        []*rtcListener
	// client, when set, is a load generator's connection set: nothing is
	// listened on, and the connections are made by the client (client_netstack.go).
	client bool
	// source, when set, makes every connection stream the cache instead of
	// echoing (-mode source-zc, rtc_zc.go).
	source *xdplink.TxCache
	// files, when set, serves objects out of the cache (-mode files-rtc,
	// rtc_files.go).
	files *fileSet
	// sink makes every connection read and discard (-mode sink-rtc): the
	// receive goroutine that delivered a batch consumes it too, so the
	// buffer pool's gets and puts land on the same P — the goroutine-per-
	// connection sink frees on another goroutine and turns every pooled
	// view into a cross-P steal, which is most of the receive profile at 100G.
	sink bool
	// conns is every open connection, for the receive-error counters that
	// live on the endpoint (not the stack): segments dropped because the
	// endpoint's queue was full, zero-window episodes. A connection leaves
	// the list when it closes, its counters folded into closedErrs — the
	// first version kept every connection ever accepted, and at 58k
	// connections a second that was 6.8 GiB after thirty seconds.
	connsMu    sync.Mutex
	conns      []*rtcConn
	closedErrs rtcErrors
}

// rtcErrors sums the endpoint-level receive errors over the connections.
type rtcErrors struct{ queueDropped, zeroWindow, csumErr uint64 }

func (r *rtcServer) errors() (t rtcErrors) {
	r.connsMu.Lock()
	defer r.connsMu.Unlock()
	t = r.closedErrs
	for _, c := range r.conns {
		t.add(c.ep)
	}
	return t
}

// add folds one endpoint's receive-error counters into t.
func (t *rtcErrors) add(ep tcpip.Endpoint) {
	st, ok := ep.Stats().(*tcp.Stats)
	if !ok {
		return
	}
	t.queueDropped += st.ReceiveErrors.SegmentQueueDropped.Value()
	t.zeroWindow += st.ReceiveErrors.ZeroRcvWindowState.Value()
	t.csumErr += st.ReceiveErrors.ChecksumErrors.Value()
}

// track puts c on the open list; untrack takes it off in O(1) and keeps
// its counters. Both under connsMu.
func (r *rtcServer) track(c *rtcConn) {
	r.connsMu.Lock()
	c.idx = len(r.conns)
	r.conns = append(r.conns, c)
	r.connsMu.Unlock()
}

func (r *rtcServer) untrack(c *rtcConn) {
	r.connsMu.Lock()
	r.closedErrs.add(c.ep)
	last := len(r.conns) - 1
	if c.idx <= last && r.conns[c.idx] == c {
		r.conns[c.idx] = r.conns[last]
		r.conns[c.idx].idx = c.idx
		r.conns[last] = nil
		r.conns = r.conns[:last]
	}
	r.connsMu.Unlock()
}

// readyConn is what the ready list holds: a connection that has something
// to do. The server's rtcConn and the load generator's connection both are.
type readyConn interface {
	service(buf []byte)
	unqueue() // clear the queued mark before servicing, so a new event re-queues
}

type rtcConn struct {
	srv     *rtcServer
	ep      tcpip.Endpoint
	wq      *waiter.Queue
	entry   waiter.Entry
	queued  atomic.Bool // in a ready shard already
	urgent  atomic.Bool // in the first lane as well (promoted while queued)
	mu      sync.Mutex  // one servicer at a time, so the echo stays in order
	closed  bool
	idx     int            // place in rtcServer.conns
	pending []byte         // unsent echo data, kept when the send buffer was full
	next    int            // source: the cache frame to stream next
	held    []*buffer.View // source: views the send buffer could not take yet
	// files: the request bytes not yet parsed, the object in progress (next
	// frame, frames left, bytes left), and which replica this connection uses
	req                        []byte
	objNext, objLeft, objBytes int
	replica                    int
}

func newRTCServer(srv *server) *rtcServer {
	r := &rtcServer{srv: srv}
	r.bufs.New = func() any { b := make([]byte, 64<<10); return &b }
	return r
}

// listen binds the raw listening endpoint and starts the accept goroutine
// (accepting is rare; it can afford a wakeup).
func (r *rtcServer) listen(s *stack.Stack, addr tcpip.Address, ports []uint16) error {
	if r.client {
		return nil
	}
	for _, port := range ports {
		l := &rtcListener{}
		ep, terr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &l.wq)
		if terr != nil {
			return errors.New(terr.String())
		}
		if terr := ep.Bind(tcpip.FullAddress{NIC: 1, Addr: addr, Port: port}); terr != nil {
			ep.Close()
			return errors.New(terr.String())
		}
		if terr := ep.Listen(4096); terr != nil {
			ep.Close()
			return errors.New(terr.String())
		}
		l.ep = ep
		r.lns = append(r.lns, l)
		go r.acceptLoop(l)
	}
	return nil
}

// rtcListener is one listening endpoint and the queue its accept loop waits
// on; a server has one per port (see -ports).
type rtcListener struct {
	ep tcpip.Endpoint
	wq waiter.Queue
}

func (r *rtcServer) acceptLoop(l *rtcListener) {
	entry, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	l.wq.EventRegister(&entry)
	defer l.wq.EventUnregister(&entry)
	for {
		ep, wq, terr := l.ep.Accept(nil)
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
				<-ch
				continue
			}
			if !r.srv.stopping.Load() {
				log.Printf("rtc accept: %v", terr)
			}
			return
		}
		ep.SocketOptions().SetDelayOption(false)
		c := &rtcConn{srv: r, ep: ep, wq: wq, replica: int(r.srv.accepted.Load())}
		c.entry = waiter.NewFunctionEntry(waiter.ReadableEvents|waiter.WritableEvents|waiter.EventHUp|waiter.EventErr, c.notify)
		wq.EventRegister(&c.entry)
		r.track(c)
		r.srv.accepted.Add(1)
		c.notify(waiter.ReadableEvents) // in case data beat the registration
	}
}

// notify is the waiter callback. It runs on whichever goroutine delivered
// the event, with the endpoint's lock held, so it only queues.
func (c *rtcConn) notify(m waiter.EventMask) {
	readable := m&waiter.ReadableEvents != 0
	if c.queued.CompareAndSwap(false, true) {
		c.srv.enqueue(c, readable)
		return
	}
	// Already waiting in the normal lane when a request arrives: it would
	// wait a whole cycle of that lane, tens of milliseconds under load.
	// Put it in the first lane too. The second entry finds nothing to do.
	if readable && c.urgent.CompareAndSwap(false, true) {
		c.srv.enqueue(c, true)
	}
}

func (c *rtcConn) unqueue() { c.queued.Store(false); c.urgent.Store(false) }

// enqueue puts c on the ready list — the first lane when it has something
// to read. Callers guard against double queueing with their own CAS; the
// one allowed duplicate is a queued connection promoted to the first lane
// by a request, and service is idempotent.
func (r *rtcServer) enqueue(c readyConn, readable bool) {
	r.mu.Lock()
	if readable {
		r.first = append(r.first, c)
	} else {
		r.ready = append(r.ready, c)
	}
	r.mu.Unlock()
}

// rtcBudgetTime bounds how long one drain spends servicing connections
// before it goes back to receiving, the way a NAPI poll has a budget.
// Without it a stack holding tens of thousands of ready connections spends
// hundreds of milliseconds in one pass — longer than a retransmit timeout —
// so acknowledgements sit unread in the ring while the sender times out data
// the peer already has: at 50000 connections that cost two thirds of the
// throughput (40 Gbit/s against 73) and 433k timeouts.
//
// Time, not a count of connections: what hurts is the length of the pass,
// and a count that is right for 50000 slow connections (four) delays the
// first byte of a short request behind a queue of long ones when there are
// only 2000. Under the budget a drain behaves exactly as it did before.
var rtcBudgetTime = 200 * time.Microsecond

// rtcBudget, when set, also bounds the connections one pass services. It is
// off by default: the time budget is the one that matters.
var rtcBudget = 0

// drain services ready connections, at most rtcBudget of them. It is the
// endpoint's AfterBatch hook: it runs on this stack's receive goroutine
// after each batch, and again on every poll timeout, so what it leaves
// behind waits a batch, not a timer.
func (r *rtcServer) drain() bool {
	// Two backing arrays alternate between "being filled" and "being
	// serviced"; both hand-offs happen under the lock, so several goroutines
	// draining one server (the single-stack mode) never share an array.
	r.mu.Lock()
	f := r.first
	r.first = r.spareFirst[:0]
	r.spareFirst = nil
	q := r.ready
	r.ready = r.spare[:0]
	r.spare = nil
	r.mu.Unlock()
	// One pass over both lanes, the first lane first. f and q keep their
	// own arrays; the loop below just walks them in order.
	nf := len(f)
	all := f
	if len(q) > 0 {
		if nf == 0 {
			all = q
		} else {
			all = append(f, q...) // rare under load: a request and transfers in one pass
		}
	}
	q, f = all, nil
	n := len(q)
	if rtcBudget > 0 && n > rtcBudget {
		n = rtcBudget
	}
	if n > 0 {
		var deadline int64
		if rtcBudgetTime > 0 {
			deadline = nanotime() + int64(rtcBudgetTime)
		}
		bp := r.bufs.Get().(*[]byte)
		for i, c := range q[:n] {
			c.unqueue() // a new event while we work re-queues it
			c.service(*bp)
			q[i] = nil
			// The clock is read every fourth connection: often enough to
			// bound the pass, rarely enough not to cost anything.
			if deadline != 0 && i&3 == 3 && nanotime() > deadline {
				n = i + 1
				break
			}
		}
		r.bufs.Put(bp)
	}
	r.mu.Lock()
	switch {
	case n < len(q):
		// Over budget: the rest keep their places at the heads of their
		// lanes. They are still marked queued, so no event re-adds them.
		if n < nf {
			// q is one backing array: appending onto q[n:nf] would write
			// over q[nf:] before it is read, and connections would vanish
			// from both lanes with queued still set — a hang. Copy the
			// first-lane remainder out before touching anything.
			first := make([]readyConn, 0, nf-n+len(r.first))
			first = append(first, q[n:nf]...)
			r.first = append(first, r.first...)
			r.ready = append(q[nf:], r.ready...)
		} else {
			r.ready = append(q[n:], r.ready...)
		}
	case r.spare == nil:
		if nf > 0 && len(q) == nf {
			r.spareFirst = q[:0] // the pass was the first lane alone: its array comes back
		} else {
			r.spare = q[:0]
		}
	}
	more := len(r.ready) > 0 || len(r.first) > 0
	r.mu.Unlock()
	return more
}

// service reads what has arrived and writes it back, until the receive
// queue is empty or the send buffer is full (then the remainder waits for
// the writable event).
func (c *rtcConn) service(buf []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.srv.source != nil {
		c.serveSource()
		return
	}
	if c.srv.files != nil {
		c.serveFilesRTC(buf)
		return
	}
	if c.srv.sink {
		for {
			w := tcpip.SliceWriter(buf)
			res, terr := c.ep.Read(&w, tcpip.ReadOptions{})
			if terr != nil {
				if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
					c.close()
				}
				return
			}
			c.srv.srv.bytesIn.Add(uint64(res.Count))
		}
	}
	if len(c.pending) > 0 && !c.flush() {
		return
	}
	for {
		w := tcpip.SliceWriter(buf)
		res, terr := c.ep.Read(&w, tcpip.ReadOptions{})
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
				return
			}
			c.close() // peer closed, reset, or error
			return
		}
		c.srv.srv.bytesIn.Add(uint64(res.Count))
		c.pending = append(c.pending[:0], buf[:res.Count]...)
		if !c.flush() {
			return
		}
	}
}

// flush writes c.pending, reporting false if some of it had to wait.
func (c *rtcConn) flush() bool {
	for len(c.pending) > 0 {
		n, terr := c.ep.Write(bytes.NewReader(c.pending), tcpip.WriteOptions{})
		if n > 0 {
			c.srv.srv.bytesOut.Add(uint64(n))
			c.pending = c.pending[n:]
		}
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
				return false
			}
			c.close()
			return false
		}
	}
	c.pending = c.pending[:0]
	return true
}

func (c *rtcConn) close() {
	if c.closed {
		return
	}
	c.closed = true
	c.wq.EventUnregister(&c.entry)
	c.srv.untrack(c) // counters first: they are read from the endpoint
	c.ep.Close()
	c.srv.srv.closed.Add(1)
}

func (r *rtcServer) stop() {
	for _, l := range r.lns {
		l.ep.Close()
	}
}
