//go:build linux

package main

import (
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"
)

// Backend is one upstream server. MAC is the next hop the rewritten packet is
// sent to; this example does not do ARP, so it comes from the command line.
//
// active is how many flows are currently pinned here, which is what backend
// selection compares. drained means RemoveBackend has taken it out of rotation
// but flows still point at it. Both are read and written only under Table.mu,
// which is why there is no accessor: handing out a *Backend would let a caller
// read them without it, and a stats goroutine doing exactly that is a data
// race. Table.Backends returns copies instead.
type Backend struct {
	Addr    netip.AddrPort
	MAC     [6]byte
	active  int
	drained bool
}

// BackendStat is a point-in-time copy of one backend, safe to read outside the
// table's lock precisely because it is a copy.
type BackendStat struct {
	Addr    netip.AddrPort
	Active  int
	Drained bool
}

// key identifies one direction of one flow. Both directions use the same
// shape, which is what makes the reverse lookup a plain map hit:
//
//	forward   {TCP, client,  cport, VIP,    vport}
//	reverse   {TCP, backend, bport, NAT-IP, natport}
type key struct {
	proto        uint8
	src, dst     netip.Addr
	sport, dport uint16
}

// flow is one translated connection. It owns its NAT port until it expires.
type flow struct {
	fwd, rev key
	backend  *Backend
	natPort  uint16

	// clientMAC is where return traffic goes. Taken from the source address of
	// the first packet, which is the router or host that handed it to us, so
	// the reply retraces the path the request arrived on. That is why this
	// example needs no ARP for the client direction.
	clientMAC [6]byte

	created, lastSeen time.Time

	// fins records which side has sent a FIN: finClient or finBackend. Both
	// means the connection is closing and can be dropped after a short drain
	// rather than held for the full idle timeout.
	fins uint8

	// dead is set when a reset ended this flow. It is reaped by the next
	// Expire rather than removed on the spot, because the packet that killed
	// it still has to be translated and sent, and that packet carries the NAT
	// port: freeing the port here would let the very next flow allocate it
	// while the reset is still in flight, and the reset would then arrive on
	// somebody else's connection.
	dead bool
}

// Which side has sent a FIN. Both bits set means the connection is closing.
const (
	finClient  = 1
	finBackend = 2
)

// Table is the NAT state: the backend set, the flow map in both directions,
// and the pool of source ports flows are translated onto.
//
// Everything is under one mutex. That is the right trade for an example: it is
// obviously correct, and it keeps the interesting part (the translation) free
// of concurrency questions. A real one would shard by flow hash, or give each
// queue its own slice of the port range so the directions never contend.
type Table struct {
	mu       sync.Mutex
	backends []*Backend
	fwd      map[key]*flow
	rev      map[key]*flow
	ports    *portPool
	natIP    netip.Addr

	// live is scratch reused by pick, so choosing a backend allocates nothing
	// on the packet path.
	live []*Backend

	// Counters, reported by the stats loop.
	created, expired, reset, noPort uint64
}

// NewTable makes an empty table translating onto natIP, using source ports in
// [portLo, portHi].
func NewTable(natIP netip.Addr, portLo, portHi uint16) *Table {
	return &Table{
		fwd:   make(map[key]*flow),
		rev:   make(map[key]*flow),
		ports: newPortPool(portLo, portHi),
		natIP: natIP,
	}
}

// AddBackend puts a server into rotation. Adding one that is already known
// returns the existing entry, and puts it back into rotation if it was
// draining.
//
// Reviving the existing entry rather than allocating a fresh one is the whole
// point: flows hold a *Backend, so a second object for the same address would
// carry a flow count of zero while the first still held every established
// connection. Selection would then see an idle server and pile new flows onto
// the busiest one. Health checks flap, so this is the ordinary case, not a
// corner.
func (t *Table) AddBackend(addr netip.AddrPort, mac [6]byte) *Backend {
	t.mu.Lock()
	defer t.mu.Unlock()
	if b := t.find(addr); b != nil {
		b.drained = false
		b.MAC = mac
		return b
	}
	b := &Backend{Addr: addr, MAC: mac}
	t.backends = append(t.backends, b)
	return b
}

// find returns the known backend for addr, draining or not. Caller holds t.mu.
func (t *Table) find(addr netip.AddrPort) *Backend {
	for _, b := range t.backends {
		if b.Addr == addr {
			return b
		}
	}
	return nil
}

// RemoveBackend takes a server out of rotation and reports whether it was
// there. Flows already on it keep running: a backend that stops receiving NEW
// connections is a drain, and tearing down established ones would be a reset
// storm. They go away on their own as they close or time out.
func (t *Table) RemoveBackend(addr netip.AddrPort) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.find(addr)
	if b == nil || b.drained {
		return false
	}
	b.drained = true
	if b.active == 0 {
		t.forget(b)
	}
	return true
}

// forget drops a drained backend once nothing points at it any more. Caller
// holds t.mu.
func (t *Table) forget(b *Backend) {
	for i, x := range t.backends {
		if x == b {
			t.backends = append(t.backends[:i], t.backends[i+1:]...)
			return
		}
	}
}

// Backends returns a snapshot of every known backend, draining ones included,
// as copies. Copies rather than pointers on purpose: a caller reading a live
// *Backend outside t.mu would race the datapath updating its flow count.
func (t *Table) Backends() []BackendStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]BackendStat, len(t.backends))
	for i, b := range t.backends {
		out[i] = BackendStat{Addr: b.Addr, Active: b.active, Drained: b.drained}
	}
	return out
}

// pick chooses a backend by the power of two choices: sample two at random and
// take the one carrying fewer flows.
//
// It is barely more code than round robin and behaves far better when flows
// are unequal, which real ones are. Round robin balances the COUNT of
// assignments; this balances what is actually outstanding, so one backend that
// accumulates long-lived connections stops attracting new ones.
//
// Caller holds t.mu.
func (t *Table) pick() *Backend {
	// Draining backends are still in the slice, holding their flows, but must
	// not take new ones.
	live := t.live[:0]
	for _, b := range t.backends {
		if !b.drained {
			live = append(live, b)
		}
	}
	t.live = live
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	// Sampled with replacement, so the two draws can be the same backend. That
	// is the standard formulation and it costs nothing here: the split across
	// equal backends is close to even over many flows, not exactly even for
	// any particular few.
	a := live[rand.IntN(len(live))]
	b := live[rand.IntN(len(live))]
	if b.active < a.active {
		return b
	}
	return a
}

// Forward looks up (or creates) the flow for a client packet heading to the
// VIP, and returns it ready to translate. ok is false when there is no state
// and none can be made: no backends, no free NAT port, or a mid-connection
// packet for a flow we never saw start.
func (t *Table) Forward(p pkt, clientMAC [6]byte, now time.Time) (*flow, bool) {
	k := key{proto: protoTCP, src: p.src, sport: p.sport, dst: p.dst, dport: p.dport}

	t.mu.Lock()
	defer t.mu.Unlock()

	if f, ok := t.fwd[k]; ok {
		t.touch(f, p, now, true)
		return f, true
	}
	// Only a SYN starts a flow. A stray ACK for something we have no state for
	// is either a flow that expired underneath us or someone else's traffic;
	// inventing a mapping for it would send it to an arbitrary backend, which
	// answers with a reset anyway.
	if p.flags&flagSYN == 0 || p.flags&flagACK != 0 {
		return nil, false
	}
	b := t.pick()
	if b == nil {
		return nil, false
	}
	port, ok := t.ports.get()
	if !ok {
		t.noPort++
		return nil, false
	}
	f := &flow{
		fwd:     k,
		rev:     key{proto: protoTCP, src: b.Addr.Addr(), sport: b.Addr.Port(), dst: t.natIP, dport: port},
		backend: b, natPort: port, clientMAC: clientMAC,
		created: now, lastSeen: now,
	}
	t.fwd[k] = f
	t.rev[f.rev] = f
	b.active++
	t.created++
	return f, true
}

// Reverse finds the flow a backend's reply belongs to. There is no creating
// here: a reply with no state is a reply to a flow that has gone.
func (t *Table) Reverse(p pkt, now time.Time) (*flow, bool) {
	k := key{proto: protoTCP, src: p.src, sport: p.sport, dst: p.dst, dport: p.dport}

	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.rev[k]
	if !ok {
		return nil, false
	}
	t.touch(f, p, now, false)
	return f, true
}

// touch refreshes a flow and folds in what the TCP flags say about its life.
// A reset ends it immediately; a FIN from each side means it is closing.
//
// Caller holds t.mu.
func (t *Table) touch(f *flow, p pkt, now time.Time, fromClient bool) {
	f.lastSeen = now
	if p.flags&flagRST != 0 {
		// Marked, not removed. The reset still has to be translated and sent,
		// and it carries this flow's NAT port; returning the port to the pool
		// here would let the next flow take it while the reset is in flight.
		// The next Expire reaps it, at most a second later.
		if !f.dead {
			f.dead = true
			t.reset++
		}
		return
	}
	if p.flags&flagFIN != 0 {
		if fromClient {
			f.fins |= finClient
		} else {
			f.fins |= finBackend
		}
	}
}

// remove deletes a flow and gives its NAT port back. Caller holds t.mu.
func (t *Table) remove(f *flow) {
	// By identity, not merely by presence: releasing another flow's port
	// because it happens to share a key would be far worse than a leak.
	if t.fwd[f.fwd] != f {
		return
	}
	delete(t.fwd, f.fwd)
	delete(t.rev, f.rev)
	t.ports.put(f.natPort)
	f.backend.active--
	if f.backend.drained && f.backend.active == 0 {
		t.forget(f.backend)
	}
	t.expired++
}

// Expire drops flows that have gone quiet and returns how many went.
//
// Two timeouts, because a closing connection and an idle one deserve different
// patience: once both sides have sent a FIN the flow only needs to outlive the
// final ACK, while an established connection with nothing to say may be idle
// for minutes and still be real.
func (t *Table) Expire(now time.Time, idle, drain time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, f := range t.fwd {
		if f.dead { // reset: reaped on the first tick after the packet went out
			t.remove(f)
			n++
			continue
		}
		limit := idle
		if f.fins == finClient|finBackend {
			limit = drain
		}
		if now.Sub(f.lastSeen) >= limit {
			t.remove(f)
			n++
		}
	}
	return n
}

// Len is the number of active flows.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.fwd)
}

// TableStats is a snapshot of the table's counters.
type TableStats struct {
	Active     uint64 // flows in the table right now
	Created    uint64 // flows ever created
	Expired    uint64 // flows ever removed, resets included
	Reset      uint64 // of which ended by a TCP reset
	NoPort     uint64 // flows refused because the NAT port pool was empty
	FreePorts  int    // NAT ports available
	TotalPorts int    // NAT ports in the range
}

// Stats snapshots the table's counters.
func (t *Table) Stats() TableStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TableStats{
		Active:     uint64(len(t.fwd)),
		Created:    t.created,
		Expired:    t.expired,
		Reset:      t.reset,
		NoPort:     t.noPort,
		FreePorts:  t.ports.avail(),
		TotalPorts: cap(t.ports.free),
	}
}

// portPool hands out NAT source ports and takes them back.
//
// A channel makes it a FIFO, which matters: a port returned by a closing flow
// goes to the back of the queue, so it is not immediately handed to a new flow
// that a straggling packet from the old one could then land on.
type portPool struct{ free chan uint16 }

func newPortPool(lo, hi uint16) *portPool {
	p := &portPool{free: make(chan uint16, int(hi)-int(lo)+1)}
	for port := int(lo); port <= int(hi); port++ {
		p.free <- uint16(port)
	}
	return p
}

func (p *portPool) get() (uint16, bool) {
	select {
	case port := <-p.free:
		return port, true
	default:
		return 0, false
	}
}

func (p *portPool) put(port uint16) {
	select {
	case p.free <- port:
	default: // cannot happen: the pool is sized for every port it owns
	}
}

func (p *portPool) avail() int { return len(p.free) }
