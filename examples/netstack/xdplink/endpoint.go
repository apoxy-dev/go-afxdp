//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package xdplink

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atoonk/go-afxdp"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/stack/gro"
)

// TxMode says how the stack's transmit calls reach the tx rings. A Socket's
// transmit side is built for one goroutine (see the afxdp package doc), and
// the stack transmits from many: application writers, TCP processors sending
// ACKs, timers retransmitting.
type TxMode int

const (
	// TxFIFO expects to sit under gVisor's fifo queueing discipline, created
	// with as many queues as the fleet has sockets:
	//
	//	fifo.New(ep, ep.NumQueues(), queueLen)
	//
	// fifo runs one goroutine per queue, assigns packets by pkt.Hash modulo
	// the queue count, and drains up to fifo.BatchSize packets into one
	// WritePackets call. The endpoint picks the socket by the same modulo, so
	// every socket's transmit side is driven by exactly one goroutine — no
	// lock — and gets batches. The cost is one goroutine handoff per packet.
	TxFIFO TxMode = iota

	// TxDirect has no queueing discipline: the stack calls WritePackets on
	// whatever goroutine produced the packet, one packet at a time, and the
	// endpoint serializes with a per-socket mutex. No handoff, no batching.
	TxDirect
)

// Config configures an Endpoint. MAC is the address frames are sent from and
// that inbound frames are classified against; VLAN, when nonzero, is the
// 802.1Q tag every frame must carry (and every transmitted frame gets); MTU
// is the IP MTU (default 1500). GRO enables gVisor's software receive
// offload, which coalesces a burst of TCP segments of one flow into a single
// packet before it enters the stack.
type Config struct {
	MAC    net.HardwareAddr
	VLAN   uint16
	MTU    uint32
	TxMode TxMode
	GRO    bool
	// GSO makes the endpoint claim segmentation offload, so TCP hands it one
	// packet of up to 64 KiB and the endpoint splits it into MSS-sized frames.
	// See gso.go.
	GSO bool
	// AfterBatch, when set, is called by every receive goroutine after each
	// batch of frames has been delivered to the stack (and, with nothing
	// arriving, about once a millisecond). An application that runs to
	// completion on the receive goroutine services its ready connections
	// here, right after the stack has processed their segments on this same
	// goroutine, with no wakeup in between. It runs concurrently on every
	// queue's goroutine.
	//
	// It reports whether it left work behind: while it does, the loop polls
	// without blocking, so an application that bounds its own work per pass
	// (rtc.go's budget) continues it at once instead of waiting for the next
	// packet or for the timeout.
	AfterBatch func() (more bool)
	// DeferKicks makes WritePackets queue frames without the transmit kick;
	// each receive goroutine kicks its own queue's socket once after its
	// batch (and AfterBatch) instead: one syscall per batch instead of one
	// per packet. Set it only when the goroutine that transmits on a socket
	// is that socket's receive loop — one socket per endpoint, or a stack
	// that transmits a connection on the queue its segments arrive on (the
	// fork's tx-hash-from-rx). Otherwise a kick waits for the owning loop's
	// next wake, at most about a millisecond, and a kick from another CPU
	// would have been faster.
	DeferKicks bool
	// TxChecksumOffload asks the NIC to finish TCP checksums instead of
	// computing them here, through transmit metadata (the sockets must have
	// been opened with afxdp.WithTxMetadata, and the driver must honour it —
	// afxdp.QueryXSKFeatures says). The stack already leaves every segment
	// with exactly the partial checksum the hardware wants.
	TxChecksumOffload bool
	// GROHold lets a merge in progress span receive batches: instead of
	// flushing GRO after every batch, held packets are delivered when they
	// have waited this long, when a flag (PSH, FIN) ends them, when they
	// reach 64 KiB, or when the loop goes idle. Zero flushes per batch. At
	// 10G a batch is a handful of frames, and a merge that stops at the
	// batch boundary means an ACK per handful — which is the peer's cost.
	GROHold time.Duration
	// TxInflight caps the frames a socket may have in the transmit ring
	// awaiting completion. Packets beyond it wait in the endpoint and go out
	// as completions arrive, in order, so nothing is dropped. This is the
	// kernel's byte-queue-limit idea for AF_XDP: a driver whose hardware
	// ring is full keeps its NAPI poll busy-waiting for it to drain (ixgbe
	// spins at ~8M polls a second per queue, a core of softirq each), and
	// the only way to keep the hardware ring from filling is not to offer
	// more than it holds. Zero means no cap: the ring's own depth, and a
	// sender with a deep window burns those cores.
	TxInflight int
	// TxBacklogHigh and TxBacklogLow bound a socket's backlog of packets
	// waiting for ring room, in packets. At or above High, WritePackets
	// accepts nothing and returns ErrTxBacklog, taking no reference: the
	// stack keeps the packet and sends it again when told there is room —
	// the way Linux's TCP small queues push back into the sender instead of
	// letting every connection pile up behind one socket until its round
	// trip passes the retransmit timeout. After every batch that leaves the
	// backlog below High the receive goroutine calls OnTxRoom, with no lock
	// held, so the stack can give the connections it holds back a turn;
	// the stack keeps that cheap when none wait. Low only marks where a
	// refusal episode is counted as over (TxResumed). Zero means the old
	// behaviour: a backlog capped by drops.
	TxBacklogHigh, TxBacklogLow int
	OnTxRoom                    func()
	// TxCache, when set, is content in the fleet's shared UMEM that packets
	// may carry without a copy; see txcache.go.
	TxCache *TxCache
	// TrustRxChecksums skips the software validation of received IPv4 and
	// TCP checksums that GRO needs (it is told they were validated). The NIC
	// did validate them, but XDP does not carry its verdict, so trusting is
	// trusting the link: a corrupted frame would be delivered. A measurement
	// knob; the honest default verifies.
	TrustRxChecksums bool
	// PinCPUs, when set, pins queue i's receive goroutine to PinCPUs[i %
	// len(PinCPUs)] itself, for an operator who places threads and
	// interrupts by hand (go-afxdp's own placement off). A receive
	// goroutine on the same core as its queue's interrupt receives almost
	// nothing, so put them on different cores.
	PinCPUs []int
}

// Stats is a snapshot of the endpoint's counters. Rx and Tx count frames
// delivered to the stack and put on the wire; the rest are drops, each named
// for its reason.
type Stats struct {
	Rx, Tx        uint64
	TxDropped     uint64 // frames the socket accepted fewer of than planned (should stay 0)
	TxDeferred    uint64 // packets that found no ring room and waited in the endpoint (sent later, in order)
	GSOPackets    uint64 // packets larger than one MSS that the endpoint split
	GSOFrames     uint64 // frames those packets became
	GROFrames     uint64 // frames handed to GRO
	GROPackets    uint64 // packets GRO delivered to the stack (frames/packets = merge ratio)
	RxBadCsum     uint64 // received frames dropped for a bad IPv4 or TCP checksum (GRO path)
	CsumOffloaded uint64 // transmitted frames whose TCP checksum the NIC finished
	CsumSoftware  uint64 // transmitted frames whose TCP checksum was computed here
	TxZeroCopy    uint64 // transmitted frames sent from the TxCache in place, no copy
	TxRefused     uint64 // WritePackets calls refused because the socket backlog was full (TxBacklogHigh)
	TxResumed     uint64 // times a backlog that had refused fell back below TxBacklogLow
	WrongVLAN     uint64 // frame tagged for another VLAN, or tagged/untagged mismatch
	NotIPv4       uint64 // EtherType the stack is not given (the filter should prevent this)
	Runt          uint64 // frame too short for an IPv4 header
	UnknownMAC    uint64 // transmit to a peer never heard from: sent to broadcast
	Neighbours    int    // size of the learned MAC table
}

// Endpoint is the stack.LinkEndpoint. Create one with New, hand it to
// stack.CreateNICWithOptions, and it starts its receive goroutines when the
// stack attaches to it.
type Endpoint struct {
	socks  []*afxdp.Socket
	cfg    Config
	mac    [6]byte
	hdrLen int
	mtu    atomic.Uint32
	neigh  *neighbours
	txq    []txQueue // per-socket transmit state

	mu         sync.Mutex
	dispatcher stack.NetworkDispatcher
	stopFD     int
	stopping   atomic.Bool
	wg         sync.WaitGroup
	onClose    func()

	rx, tx, txDropped, wrongVLAN, notIPv4, runt, unknownMAC atomic.Uint64
	gsoPackets, gsoFrames, groFrames, groPackets            atomic.Uint64
	rxChecksumBad                                           atomic.Uint64
	csumOffloaded                                           atomic.Uint64 // frames whose TCP checksum the NIC finishes
	csumSoftware                                            atomic.Uint64 // frames whose TCP checksum was computed here
	txZeroCopy                                              atomic.Uint64 // frames transmitted from the cache in place
	txRefused                                               atomic.Uint64 // WritePackets calls refused for a full backlog
	txResumed                                               atomic.Uint64 // refused-to-room transitions
	txQueued                                                atomic.Uint64 // packets that had to wait for ring room
}

// txQueue is one socket's transmit side: the serialisation the socket
// needs, the packets waiting for room, and the in-flight count TxInflight
// bounds. In fifo mode one goroutine per socket owns it and the lock is
// uncontended; in direct mode every writer takes it.
type txQueue struct {
	mu          sync.Mutex
	pending     []*stack.PacketBuffer // owned references, oldest first
	urgent      []*stack.PacketBuffer // control packets (see isControl), sent ahead of pending
	segs        []int                 // scratch for the plan pass
	zc          []bool                // scratch: packet p goes out of the cache in place
	descs       []afxdp.Desc          // scratch: cache descriptors
	pieces      []piece               // scratch: the pieces of every packet in the batch, in order
	pieceEnd    []int                 // scratch: where packet p's pieces end in pieces
	kickPending atomic.Bool           // a deferred kick is owed (DeferKicks)
	refused     atomic.Bool           // a writer was refused since the backlog last fell below low water
	copy        copier                // transmitCopied's builder, reused
	// nPending and nUrgent mirror len(pending) and len(urgent) for the
	// receive loop's lock-free peek (flushPending); updated under mu by
	// note().
	nPending, nUrgent atomic.Int32
}

// note publishes the backlog lengths for readers that hold no lock.
func (q *txQueue) note() {
	q.nPending.Store(int32(len(q.pending)))
	q.nUrgent.Store(int32(len(q.urgent)))
}

// maxPending bounds a socket's backlog in packets when no TxBacklogHigh is
// set; beyond it new packets are dropped and TCP retransmits.
const maxPending = 8192

// controlMax is the largest packet treated as control: acknowledgements,
// SYN-ACKs, window probes and updates. Those are never refused by the
// backlog limit and go ahead of the data waiting in it, the way a kernel's
// small queues bound data per socket but never hold back an ACK: a refused
// SYN-ACK is a second of handshake timeout at the peer, a refused ACK a
// stalled sender. FIN and RST stay in order behind their connection's data.
const controlMax = 128

// isControl reports whether pkt is a small packet that may go ahead of the
// data backlog.
func isControl(pkt *stack.PacketBuffer) bool {
	if pkt.Size() > controlMax {
		return false
	}
	if h := pkt.TransportHeader().Slice(); pkt.TransportProtocolNumber == header.TCPProtocolNumber && len(h) >= header.TCPMinimumSize {
		return header.TCP(h).Flags()&(header.TCPFlagFin|header.TCPFlagRst) == 0
	}
	return true
}

// ErrTxBacklog is what WritePackets returns when the socket's backlog is at
// TxBacklogHigh: nothing was queued, the stack keeps its packets, and it
// will be told through OnTxRoom when to try again. It is a WouldBlock so
// that a stack which understands that (the fork) pauses the sender rather
// than counting a loss.
var ErrTxBacklog tcpip.Error = &tcpip.ErrWouldBlock{}

var _ stack.LinkEndpoint = (*Endpoint)(nil)

// New builds an Endpoint over every socket in fleet. The fleet must have been
// opened with a filter that delivers only the stack's own traffic (a TCP
// port); see the package doc for why ARP must stay with the kernel.
func New(fleet *afxdp.Fleet, cfg Config) (*Endpoint, error) {
	return NewForSockets(fleet.Sockets(), cfg)
}

// NewForSockets builds an Endpoint over a subset of a fleet's sockets — one,
// for a stack per queue, where each stack owns exactly one receive goroutine
// and one transmit ring and shares nothing with its neighbours. The fleet
// still owns the XDP program and the queues; RSS decides which stack a
// connection lands on and keeps it there.
func NewForSockets(socks []*afxdp.Socket, cfg Config) (*Endpoint, error) {
	if len(cfg.MAC) != 6 {
		return nil, fmt.Errorf("xdplink: MAC must be 6 bytes, got %v", cfg.MAC)
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if len(socks) == 0 {
		return nil, errors.New("xdplink: no sockets")
	}
	hdrLen := linkHeaderLen(cfg.VLAN)
	if fs := socks[0].FrameSize() - socks[0].TxMetadataLen(); int(cfg.MTU)+hdrLen > fs {
		return nil, fmt.Errorf("xdplink: MTU %d plus %d-byte link header exceeds the %d bytes a UMEM frame holds",
			cfg.MTU, hdrLen, fs)
	}
	if cfg.TxChecksumOffload && socks[0].TxMetadataLen() == 0 {
		return nil, errors.New("xdplink: TxChecksumOffload needs sockets opened with afxdp.WithTxMetadata")
	}
	e := &Endpoint{
		socks:  socks,
		cfg:    cfg,
		hdrLen: hdrLen,
		neigh:  newNeighbours(),
		txq:    make([]txQueue, len(socks)),
		stopFD: -1,
	}
	copy(e.mac[:], cfg.MAC)
	e.mtu.Store(cfg.MTU)
	return e, nil
}

// NumQueues is how many sockets the endpoint transmits on: the queue count
// to give fifo.New in TxFIFO mode.
func (e *Endpoint) NumQueues() int { return len(e.socks) }

// Stats returns a snapshot of the counters.
func (e *Endpoint) Stats() Stats {
	return Stats{
		Rx: e.rx.Load(), Tx: e.tx.Load(), TxDropped: e.txDropped.Load(),
		TxDeferred: e.txQueued.Load(), GSOPackets: e.gsoPackets.Load(), GSOFrames: e.gsoFrames.Load(),
		GROFrames: e.groFrames.Load(), GROPackets: e.groPackets.Load(),
		RxBadCsum: e.rxChecksumBad.Load(), CsumOffloaded: e.csumOffloaded.Load(), CsumSoftware: e.csumSoftware.Load(), TxZeroCopy: e.txZeroCopy.Load(), TxRefused: e.txRefused.Load(), TxResumed: e.txResumed.Load(),
		WrongVLAN: e.wrongVLAN.Load(), NotIPv4: e.notIPv4.Load(), Runt: e.runt.Load(),
		UnknownMAC: e.unknownMAC.Load(), Neighbours: e.neigh.len(),
	}
}

// ---- stack.NetworkLinkEndpoint ----

// MTU implements stack.LinkEndpoint.
func (e *Endpoint) MTU() uint32 { return e.mtu.Load() }

// SetMTU implements stack.LinkEndpoint.
func (e *Endpoint) SetMTU(mtu uint32) { e.mtu.Store(mtu) }

// MaxHeaderLength implements stack.LinkEndpoint: the Ethernet header, tagged
// or not, that AddHeader will push.
func (e *Endpoint) MaxHeaderLength() uint16 { return uint16(e.hdrLen) }

// LinkAddress implements stack.LinkEndpoint.
func (e *Endpoint) LinkAddress() tcpip.LinkAddress { return tcpip.LinkAddress(e.mac[:]) }

// SetLinkAddress implements stack.LinkEndpoint.
func (e *Endpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	if len(addr) == 6 {
		copy(e.mac[:], addr)
	}
}

// Capabilities implements stack.LinkEndpoint. Nothing is claimed by default:
// AF_XDP does not carry the NIC's checksum verdict to userspace, so the stack
// verifies inbound checksums and computes outbound ones itself. With GRO the
// endpoint claims receive checksum offload, because GRO verifies each segment
// before merging and a merged packet's TCP checksum is stale by construction;
// the stack would otherwise reject every coalesced packet.
func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities {
	if e.cfg.GRO {
		return stack.CapabilityRXChecksumOffload
	}
	return stack.CapabilityNone
}

// GSOMaxSize implements stack.GSOEndpoint: the largest packet TCP may hand
// WritePackets when GSO is on.
func (e *Endpoint) GSOMaxSize() uint32 { return gsoMaxSize }

// SupportedGSO implements stack.GSOEndpoint. HostGSOSupported is the flavour
// where the stack sends one big packet and the link splits it; the other
// flavour (GVisorGSOSupported) splits in the TCP layer and would hand us the
// same per-MSS packets as no GSO at all.
func (e *Endpoint) SupportedGSO() stack.SupportedGSO {
	if e.cfg.GSO {
		return stack.HostGSOSupported
	}
	return stack.GSONotSupported
}

var _ stack.GSOEndpoint = (*Endpoint)(nil)

// ARPHardwareType implements stack.LinkEndpoint.
func (e *Endpoint) ARPHardwareType() header.ARPHardwareType { return header.ARPHardwareEther }

// Attach implements stack.LinkEndpoint. A non-nil dispatcher starts one
// receive goroutine per socket and returns once each has pinned itself to its
// queue's CPU, so that no later transmit from a stack goroutine can claim the
// pin instead. A nil dispatcher stops the goroutines and waits for them.
func (e *Endpoint) Attach(d stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if d == nil {
		e.stopLocked()
		return
	}
	if e.dispatcher != nil {
		return
	}
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		panic(fmt.Sprintf("xdplink: eventfd: %v", err))
	}
	e.stopFD = efd
	e.stopping.Store(false)
	e.dispatcher = d

	var pinned sync.WaitGroup
	for qi, xsk := range e.socks {
		e.wg.Add(1)
		pinned.Add(1)
		go e.rxLoop(qi, xsk, d, &pinned)
	}
	pinned.Wait()
}

func (e *Endpoint) stopLocked() {
	if e.dispatcher == nil {
		return
	}
	e.stopping.Store(true)
	one := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	if _, err := unix.Write(e.stopFD, one); err != nil {
		panic(fmt.Sprintf("xdplink: write eventfd: %v", err))
	}
	e.wg.Wait()
	unix.Close(e.stopFD)
	e.stopFD = -1
	e.dispatcher = nil
	// Nothing transmits any more; give the backlog's references back.
	for i := range e.txq {
		q := &e.txq[i]
		q.mu.Lock()
		for _, pkt := range q.pending {
			pkt.DecRef()
		}
		for _, pkt := range q.urgent {
			pkt.DecRef()
		}
		q.pending, q.urgent = nil, nil
		q.note()
		q.mu.Unlock()
	}
}

// IsAttached implements stack.LinkEndpoint.
func (e *Endpoint) IsAttached() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dispatcher != nil
}

// Wait implements stack.LinkEndpoint: it blocks until the receive goroutines
// have exited.
func (e *Endpoint) Wait() { e.wg.Wait() }

// Close implements stack.LinkEndpoint.
func (e *Endpoint) Close() {
	e.mu.Lock()
	e.stopLocked()
	f := e.onClose
	e.mu.Unlock()
	if f != nil {
		f()
	}
}

// SetOnCloseAction implements stack.LinkEndpoint.
func (e *Endpoint) SetOnCloseAction(f func()) {
	e.mu.Lock()
	e.onClose = f
	e.mu.Unlock()
}

// AddHeader implements stack.LinkEndpoint: it prepends the Ethernet (and
// 802.1Q) header. The destination MAC is the route's, when the stack resolved
// one, and otherwise comes from the learned table by IPv4 destination.
func (e *Endpoint) AddHeader(pkt *stack.PacketBuffer) {
	dst := broadcastMAC
	if r := pkt.EgressRoute.RemoteLinkAddress; len(r) == 6 {
		copy(dst[:], r)
	} else if nh := pkt.NetworkHeader().Slice(); len(nh) >= ipv4MinHdrLen {
		if mac, ok := e.neigh.lookup([4]byte(nh[16:20])); ok {
			dst = mac
		} else {
			e.unknownMAC.Add(1)
		}
	} else {
		e.unknownMAC.Add(1)
	}
	hdr := pkt.LinkHeader().Push(e.hdrLen)
	putHeader(hdr, dst, e.mac, e.cfg.VLAN, uint16(pkt.NetworkProtocolNumber))
}

// ParseHeader implements stack.LinkEndpoint.
func (e *Endpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	_, ok := pkt.LinkHeader().Consume(e.hdrLen)
	return ok
}

// ---- receive ----

// rxLoop is one queue's goroutine: Fill, Poll, Receive, deliver, Recycle. It
// holds no frame past Recycle, so the fill ring is always fed and an
// indefinite Poll is safe. The stop eventfd is polled alongside the socket.
func (e *Endpoint) rxLoop(qi int, xsk *afxdp.Socket, d stack.NetworkDispatcher, pinned *sync.WaitGroup) {
	defer e.wg.Done()
	if len(e.cfg.PinCPUs) > 0 {
		runtime.LockOSThread()
		var set unix.CPUSet
		set.Zero()
		set.Set(e.cfg.PinCPUs[xsk.QueueID()%len(e.cfg.PinCPUs)]) // by NIC queue, not loop index: one socket per endpoint is the norm
		_ = unix.SchedSetaffinity(0, &set)
	} else {
		_, _ = xsk.Pin() // best effort; PinError on the socket says why if not
	}
	pinned.Done()

	var g *gro.GRO
	if e.cfg.GRO {
		g = &gro.GRO{Dispatcher: countingDispatcher{d: d, n: &e.groPackets}}
		g.Init(true)
	}
	extra := []int32{int32(e.stopFD)}
	after := e.cfg.AfterBatch
	timeout := time.Duration(-1)
	if after != nil {
		timeout = time.Millisecond // so a connection made ready elsewhere is not left waiting
	}
	if e.cfg.DeferKicks && timeout < 0 {
		timeout = time.Millisecond // a deferred kick must not wait on the next packet
	}
	hold := int64(e.cfg.GROHold)
	if g == nil {
		hold = 0
	}
	if hold > 0 && timeout < 0 {
		timeout = time.Millisecond // a held merge must not wait on the next packet either
	}
	heldSince := int64(-1) // when the oldest packet still in GRO arrived, or -1
	more := false          // AfterBatch has work left: do not block on the poll
	kick := func() {
		if e.cfg.DeferKicks && e.txq[qi].kickPending.Swap(false) {
			_ = xsk.KickIfNeeded()
		}
	}
	for {
		xsk.Fill(xsk.NumFreeFillSlots())
		t := timeout
		if more {
			t = 0 // work is waiting; look for packets but do not wait for them
		}
		if _, err := xsk.PollWith(extra, t); err != nil {
			return
		}
		if e.stopping.Load() {
			return
		}
		n := xsk.NumReceived()
		if n == 0 {
			if heldSince >= 0 {
				g.Flush() // idle: nothing more is coming to merge with
				heldSince = -1
			}
			if after != nil {
				more = after()
			}
			e.flushPending(qi)
			kick()
			continue
		}
		descs := xsk.Receive(n)
		for _, dsc := range descs {
			e.deliver(qi, xsk.GetFrame(dsc), d, g)
		}
		if g != nil {
			if hold == 0 {
				g.Flush()
			} else {
				now := time.Now().UnixNano()
				if heldSince < 0 {
					heldSince = now
				}
				if now-heldSince >= hold {
					g.Flush()
					heldSince = -1
				}
			}
		}
		xsk.Recycle(descs)
		if after != nil {
			more = after()
		}
		e.flushPending(qi)
		kick()
	}
}

// flushPending sends what the socket's backlog can now fit. Completions
// arrive with the traffic, so the receive goroutine checks after every batch
// and on every timeout; a socket that is only ever written to from here is
// drained here.
func (e *Endpoint) flushPending(i int) {
	q := &e.txq[i]
	if q.nPending.Load() > 0 || q.nUrgent.Load() > 0 || q.refused.Load() { // a peek to skip the lock; re-checked under it
		q.mu.Lock()
		if len(q.pending) > 0 || len(q.urgent) > 0 {
			e.reclaim(q, e.socks[i])
			e.drainPending(q, e.socks[i])
		}
		if q.refused.Load() && len(q.pending) < e.cfg.TxBacklogLow {
			q.refused.Store(false)
			e.txResumed.Add(1)
		}
		q.mu.Unlock()
	}
	// Whenever the backlog has room for a write, the stack may have
	// endpoints waiting for a turn (see Config.OnTxRoom); the call is cheap
	// when it has none, and the stack stops at the first refusal. Waiting
	// for low water instead left a refused connection waiting for a dip
	// that under steady load came hundreds of milliseconds apart — the
	// backlog sat between the marks, topped up by everyone not blocked —
	// and that was the first byte's p99. Outside the lock: the stack
	// writes from here.
	if f := e.cfg.OnTxRoom; f != nil && e.cfg.TxBacklogHigh > 0 && int(q.nPending.Load()) < e.cfg.TxBacklogHigh {
		f()
	}
}

// reclaim takes completions back, so the budget computed next is exact.
// The in-flight count itself is the socket's (NumTransmitted): SendFunc
// reclaims on its own too, and a count kept here would miss those.
func (e *Endpoint) reclaim(q *txQueue, xsk *afxdp.Socket) {
	xsk.Complete(xsk.NumCompleted())
}

// drainPending transmits the backlog's leading packets while there is room,
// releasing the references it held. Called with q.mu held.
func (e *Endpoint) drainPending(q *txQueue, xsk *afxdp.Socket) {
	defer q.note()
	if len(q.urgent) > 0 && !e.drainList(q, xsk, &q.urgent) {
		return
	}
	e.drainList(q, xsk, &q.pending)
}

// drainList transmits the leading packets of *list while there is room and
// reports whether it emptied the list.
func (e *Endpoint) drainList(q *txQueue, xsk *afxdp.Socket, list *[]*stack.PacketBuffer) bool {
	for len(*list) > 0 {
		n, err := e.transmit(q, xsk, *list)
		for _, pkt := range (*list)[:n] {
			pkt.DecRef()
		}
		*list = append((*list)[:0], (*list)[n:]...)
		if n == 0 || err != nil {
			return false
		}
	}
	return true
}

// deliver hands one received frame to the stack. This is where the receive
// copy happens: the frame is copied into a pooled view and is free to be
// recycled the moment deliver returns.
func (e *Endpoint) deliver(qi int, f []byte, d stack.NetworkDispatcher, g *gro.GRO) {
	hdr, et, ok := parseFrame(f, e.cfg.VLAN)
	if !ok {
		e.wrongVLAN.Add(1)
		return
	}
	if et != etherTypeIPv4 {
		e.notIPv4.Add(1)
		return
	}
	if len(f) < hdr+ipv4MinHdrLen {
		e.runt.Add(1)
		return
	}
	e.rx.Add(1)
	if g == nil {
		// Without GRO the stack verifies checksums itself, later; the
		// peer's MAC must still not be learned from a frame that fails
		// them, or a corrupt or forged flood rewrites a real peer's entry.
		// (With GRO the frame is verified below, once, before learning.)
		if e.cfg.TrustRxChecksums || checksumsValid(f[hdr:]) {
			e.neigh.learn([4]byte(f[hdr+12:hdr+16]), [6]byte(f[6:12]))
		}
	}

	v := buffer.NewViewSize(len(f))
	copy(v.AsSlice(), f)
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithView(v)})
	pkt.NetworkProtocolNumber = header.IPv4ProtocolNumber
	// The queue this arrived on. A patched stack copies it into the
	// connection's transmit hash, so the reply goes out on the same socket
	// this goroutine owns; unpatched, it is only the fifo queue choice.
	pkt.Hash = uint32(qi)
	pkt.LinkHeader().Consume(hdr)
	switch {
	case [6]byte(f[0:6]) == e.mac:
		pkt.PktType = tcpip.PacketHost
	case [6]byte(f[0:6]) == broadcastMAC:
		pkt.PktType = tcpip.PacketBroadcast
	case isMulticast(f[0:6]):
		pkt.PktType = tcpip.PacketMulticast
	default:
		pkt.PktType = tcpip.PacketOtherHost
	}
	if g != nil {
		// GRO validates a packet's checksums before merging and silently
		// delivers it unmerged on failure — and upstream's check reads the
		// wrong byte range once a link header has been consumed
		// (PacketData.ChecksumAtOffset forgets the data offset; see
		// patches/README.md, 0003, now in 0011). So validate here, on the raw frame, and tell GRO.
		// A bad checksum is dropped outright: with RX offload claimed the
		// stack would otherwise accept it unverified.
		if !e.cfg.TrustRxChecksums && !checksumsValid(f[hdr:]) {
			e.rxChecksumBad.Add(1)
			pkt.DecRef()
			return
		}
		pkt.RXChecksumValidated = true
		e.neigh.learn([4]byte(f[hdr+12:hdr+16]), [6]byte(f[6:12]))
		e.groFrames.Add(1)
		g.Enqueue(pkt)
	} else {
		d.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)
	}
	pkt.DecRef() // the stack holds its own reference to what it keeps
}

// AddNeighbour seeds the learned table with the MAC for ip, for a peer the
// endpoint must send to before it has heard from it (a client's first SYN).
// The kernel's ARP table is the usual source.
func (e *Endpoint) AddNeighbour(ip [4]byte, mac [6]byte) { e.neigh.learn(ip, mac) }

// countingDispatcher sits between GRO and the stack so the merge ratio
// (frames in versus packets out) is observable.
type countingDispatcher struct {
	d stack.NetworkDispatcher
	n *atomic.Uint64
}

func (c countingDispatcher) DeliverNetworkPacket(p tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	c.n.Add(1)
	c.d.DeliverNetworkPacket(p, pkt)
}

func (c countingDispatcher) DeliverLinkPacket(p tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	c.d.DeliverLinkPacket(p, pkt)
}

// ---- transmit ----

// WritePackets implements stack.LinkWriter. Each packet is built directly
// into UMEM frames by SendFunc, which also reclaims completed frames and
// kicks the driver. The socket is chosen by pkt.Hash modulo the socket count;
// under fifo every packet of a batch shares that value (see TxFIFO).
//
// A GSO packet becomes several frames, so the frame count is planned first
// and only whole packets are admitted: a frame on the ring is on the wire,
// and half a packet on the wire is worse than none. Packets that do not fit
// are not sent at all — a counted deferral, never a wait: TCP retransmits.
func (e *Endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	list := pkts.AsSlice()
	if len(list) == 0 {
		return 0, nil
	}
	i := int(list[0].Hash) % len(e.socks)
	xsk := e.socks[i]
	q := &e.txq[i]
	q.mu.Lock()
	defer q.mu.Unlock()
	if e.stopping.Load() {
		return 0, &tcpip.ErrClosedForSend{}
	}

	e.reclaim(q, xsk)
	control := len(list) == 1 && isControl(list[0])
	if hi := e.cfg.TxBacklogHigh; hi > 0 && len(q.pending) >= hi && !control {
		// Full: try to make room first, then push back rather than queue.
		e.drainPending(q, xsk)
		if len(q.pending) >= hi {
			q.refused.Store(true)
			e.txRefused.Add(1)
			return 0, ErrTxBacklog
		}
	}
	sent := 0
	if len(q.pending) == 0 && len(q.urgent) == 0 {
		// The common case: nothing is waiting, send straight from the batch.
		n, err := e.transmit(q, xsk, list)
		if err != nil {
			return n, &tcpip.ErrClosedForSend{}
		}
		sent = n
	} else {
		// A backlog exists; the batch goes behind it (a control packet
		// ahead of it), then everything is drained in order as far as
		// room allows.
		e.drainPending(q, xsk)
	}
	// Whatever did not fit waits for completions. The stack drops its own
	// references when this returns, so the queue takes its own.
	for _, pkt := range list[sent:] {
		if control {
			if len(q.urgent) >= maxPending {
				e.txDropped.Add(1)
				continue
			}
			q.urgent = append(q.urgent, pkt.IncRef())
		} else {
			if len(q.pending) >= maxPending {
				e.txDropped.Add(1)
				continue
			}
			q.pending = append(q.pending, pkt.IncRef())
		}
		e.txQueued.Add(1)
	}
	q.note()
	return len(list), nil
}

// transmit builds as many leading whole packets of pkts into frames as the
// ring, the pool and TxInflight allow, and queues them. It returns the
// packets sent. Called with q.mu held.
func (e *Endpoint) transmit(q *txQueue, xsk *afxdp.Socket, pkts []*stack.PacketBuffer) (int, error) {
	budget := xsk.NumFreeTxSlots()
	if f := xsk.FreeTxFrames(); f < budget {
		budget = f
	}
	inflightCap := false
	if lim := e.cfg.TxInflight; lim > 0 && lim-xsk.NumTransmitted() < budget {
		budget = lim - xsk.NumTransmitted()
		inflightCap = true
	}
	if budget <= 0 {
		if xsk.NumFreeTxSlots() == 0 {
			_ = xsk.Kick() // keep the driver draining; see Socket.Kick
		}
		return 0, nil
	}

	// Plan: frames per packet, and which packets can go out of the cache in
	// place. (A cache packet needs ring slots but no pool frames; counting
	// it against both keeps the arithmetic simple and the budget exact.)
	segs, zc := q.segs[:0], q.zc[:0]
	q.pieces, q.pieceEnd = q.pieces[:0], q.pieceEnd[:0]
	for _, pkt := range pkts {
		n, inplace := e.plan(q, pkt)
		segs = append(segs, n)
		zc = append(zc, inplace)
		q.pieceEnd = append(q.pieceEnd, len(q.pieces)) // pieces of packet p end here
		if len(segs) >= budget {                       // no later packet can fit anyway
			break
		}
	}
	q.segs, q.zc = segs, zc
	admitted, _ := admit(segs, budget)
	if admitted == 0 && inflightCap && xsk.NumTransmitted() == 0 && segs[0] <= xsk.NumFreeTxSlots() && segs[0] <= xsk.FreeTxFrames() {
		// An in-flight cap below one packet's frames must not stall the
		// queue for good: with nothing in flight the head goes out whole.
		admitted = 1
	}
	if admitted == 0 {
		return 0, nil
	}

	// Emit in packet order: runs of copied packets through SendFunc, cache
	// packets as descriptors of their own. Kicks: deferred to the socket's
	// own loop when the caller promised that (see Config.DeferKicks), else
	// once here at the end.
	deferKick := e.cfg.DeferKicks
	if deferKick {
		q.kickPending.Store(true)
	}
	sent := 0
	var firstErr error
	for start := 0; start < admitted && firstErr == nil; {
		if zc[start] {
			from := 0
			if start > 0 {
				from = q.pieceEnd[start-1]
			}
			n, err := e.transmitCached(q, xsk, pkts[start], q.pieces[from:q.pieceEnd[start]])
			if err != nil || n == 0 {
				firstErr = err
				break
			}
			sent++
			start++
			continue
		}
		end := start
		frames := 0
		for end < admitted && !zc[end] {
			frames += segs[end]
			end++
		}
		n, err := e.transmitCopied(q, xsk, pkts[start:end], segs[start:end], frames, deferKick)
		sent += n
		if err != nil || n < end-start {
			firstErr = err
			break
		}
		start = end
	}
	if !deferKick && sent > 0 {
		_ = xsk.KickIfNeeded()
	}
	return sent, firstErr
}

// plan returns the frames pkt needs and whether it goes the per-piece way:
// its payload is one or more strided views over the cache (see TxCache.Span),
// each of which maps to frames — one per PayloadLen of logical bytes, in
// place when the piece starts at a frame's payload start, copied when the
// stack split a segment mid-frame. Any other payload view in such a packet
// is copied as one frame if it fits a segment; otherwise the packet takes
// the ordinary path.
func (e *Endpoint) plan(q *txQueue, pkt *stack.PacketBuffer) (frames int, perPiece bool) {
	c := e.cfg.TxCache
	if c == nil || !needsChecksum(pkt) { // cache content is TCP data from an established connection
		return segments(pkt), false
	}
	mss := int(pkt.GSOOptions.MSS)
	if pkt.GSOOptions.Type == stack.GSONone || mss == 0 {
		mss = pkt.Data().Size() // a single segment: the payload is one view or nothing
	}
	if c.plen > mss {
		// The stack would split every frame; the pieces path still handles
		// it (mid-frame pieces are copied), just without the benefit.
	}
	cur := newPayloadCursor(pkt)
	if cur.v == nil {
		return segments(pkt), false
	}
	first := len(q.pieces) // this packet's pieces start here
	external := 0
	firstView := true
	for v := cur.v; v != nil; v = v.Next() {
		if span, read, write, ok := viewExternal(v); ok {
			if firstView {
				read += cur.off
			}
			var good bool
			if q.pieces, good = c.pieces(q.pieces, span, read, write); !good {
				// Not our cache's memory: nothing here can read it. Never
				// the ordinary path, which would put its raw bytes on the
				// wire; the packet is dropped and counted.
				e.txDropped.Add(1)
				q.pieces = q.pieces[:first]
				return 0, true
			}
			external++
		} else {
			n := v.Size()
			if firstView {
				n -= cur.off
			}
			for n > 0 { // in segment-sized pieces, copied through the cursor
				k := min(n, mss)
				q.pieces = append(q.pieces, piece{frame: -1, n: k})
				n -= k
			}
		}
		firstView = false
	}
	if external == 0 {
		q.pieces = q.pieces[:first]
		return segments(pkt), false
	}
	return len(q.pieces) - first, true
}

// transmitCopied sends a run of packets through pool frames, as WritePackets
// always has. It returns the packets sent.
func (e *Endpoint) transmitCopied(q *txQueue, xsk *afxdp.Socket, pkts []*stack.PacketBuffer, segs []int, total int, deferKick bool) (int, error) {
	// SendFunc calls the builder for i = 0..total-1 in order, so a running
	// cursor over (packet, segment) is all the index that is needed. The
	// builder is a method on a struct the queue keeps: a closure here would
	// carry the segmenter to the heap on every call (one allocation per
	// packet run, a fifth of everything a churning server allocated).
	c := &q.copy
	c.e, c.xsk, c.pkts, c.segs = e, xsk, pkts, segs
	c.p, c.gso = 0, false
	if c.fn == nil {
		c.fn = c.build
	}
	send := xsk.SendFunc
	if deferKick {
		send = xsk.SendFuncNoKick
	}
	n, err := send(total, c.fn)
	c.pkts, c.segs = nil, nil // no reference kept past the call
	e.tx.Add(uint64(n))
	if n != total {
		// Not expected: the budget was exact. Frames beyond a packet boundary
		// were never queued, so account for what really went out.
		e.txDropped.Add(uint64(total - n))
	}
	sent, _ := admit(segs, n)
	return sent, err
}

// copier is transmitCopied's builder state, one per queue, reused.
type copier struct {
	e    *Endpoint
	xsk  *afxdp.Socket
	pkts []*stack.PacketBuffer
	segs []int
	p    int // packet being emitted
	s    segmenter
	gso  bool // s is initialised for packet p
	fn   func(int, []byte) int
}

// build fills one frame: the next whole packet, or the next segment of the
// packet being split.
func (c *copier) build(_ int, frame []byte) int {
	e, xsk := c.e, c.xsk
	pkt := c.pkts[c.p]
	var length int
	if c.segs[c.p] == 1 {
		length = copyPacket(frame, pkt)
		if needsChecksum(pkt) {
			if e.cfg.TxChecksumOffload {
				stampChecksumRequest(frame, xsk.TxMeta(frame), e.hdrLen)
				e.csumOffloaded.Add(1)
			} else {
				finishTCPChecksum(frame, e.hdrLen)
				e.csumSoftware.Add(1)
			}
		}
		c.p++
	} else {
		if !c.gso {
			c.s.init(pkt, e.hdrLen)
			if e.cfg.TxChecksumOffload {
				c.s.offload = xsk.TxMeta
				e.csumOffloaded.Add(uint64(c.segs[c.p]))
			} else {
				c.s.offload = nil
				e.csumSoftware.Add(uint64(c.segs[c.p]))
			}
			c.gso = true
			e.gsoPackets.Add(1)
			e.gsoFrames.Add(uint64(c.segs[c.p]))
		}
		length = c.s.next(frame)
		if c.s.done() {
			c.gso = false
			c.p++
		}
	}
	return length
}

// transmitCached sends one packet a frame per piece (see plan, which left
// the pieces in q.pieces): cache pieces at a frame's payload start go out
// in place, their headers written into the room before them; the rest are
// copied into pool frames. The ring keeps the order frames are queued in,
// so the two kinds interleave freely. Returns the packets sent (1).
func (e *Endpoint) transmitCached(q *txQueue, xsk *afxdp.Socket, pkt *stack.PacketBuffer, pieces []piece) (int, error) {
	c := e.cfg.TxCache
	npieces := len(pieces)
	if npieces == 0 {
		return 1, nil // dropped in plan; the stack retransmits
	}
	var s segmenter
	s.init(pkt, e.hdrLen)
	if e.cfg.TxChecksumOffload {
		s.offload = xsk.TxMeta
	}
	descs := q.descs[:0]
	flush := func() bool { // queue the cache descriptors gathered so far, in order
		if len(descs) == 0 {
			return true
		}
		n := xsk.TransmitNoKick(descs)
		e.tx.Add(uint64(n))
		e.txZeroCopy.Add(uint64(n))
		if n < len(descs) {
			// The ring came up short of the budget we computed: not
			// expected. Frames that never went out will never complete.
			for _, d := range descs[n:] {
				c.busy[int((d.Addr-c.base)/uint64(c.fs))].Store(false)
			}
			e.txDropped.Add(uint64(len(descs) - n))
			descs = descs[:0]
			return false
		}
		descs = descs[:0]
		return true
	}
	// Copied pieces read their bytes through the same cursor the segmenter
	// uses, so an ordinary view and a strided one are handled alike: for a
	// strided view the cursor's bytes are the span's, and a piece's bytes
	// sit at the piece's frame — so copies come from the frame, never the
	// cursor, for cache pieces.
	cur := newPayloadCursor(pkt)
	ok := true
	for _, pc := range pieces {
		if !ok {
			break
		}
		if pc.frame >= 0 && pc.inPlace && c.Acquire(pc.frame) {
			addr, frame := c.frame(pc.frame)
			room := c.off - s.hdrLen
			length := s.nextInPlace(frame[room:c.off+pc.n], pc.n)
			d := afxdp.Desc{Addr: addr + uint64(room), Len: uint32(length)}
			if xsk.TxMetadataLen() > 0 {
				d.Options = afxdp.DescTxMetadata
			}
			descs = append(descs, d)
			cur.skip(pc.n)
			continue
		}
		// Copied: a pool frame, this piece's bytes behind the headers.
		if ok = flush(); !ok {
			break
		}
		src := (func() []byte)(nil)
		if pc.frame >= 0 {
			_, frame := c.frame(pc.frame)
			p := frame[c.off+pc.off : c.off+pc.off+pc.n]
			src = func() []byte { return p }
			cur.skip(pc.n)
		}
		n, err := xsk.SendFuncNoKick(1, func(_ int, frame []byte) int {
			if src != nil {
				copy(frame[s.hdrLen:], src())
			} else {
				cur.copyTo(frame[s.hdrLen : s.hdrLen+pc.n])
			}
			return s.nextInPlace(frame[:s.hdrLen+pc.n], pc.n)
		})
		e.tx.Add(uint64(n))
		if err != nil || n != 1 {
			e.txDropped.Add(1)
			ok = false
		}
	}
	if ok {
		flush()
	}
	q.descs = descs
	if e.cfg.TxChecksumOffload {
		e.csumOffloaded.Add(uint64(npieces))
	} else {
		e.csumSoftware.Add(uint64(npieces))
	}
	if npieces > 1 {
		e.gsoPackets.Add(1)
		e.gsoFrames.Add(uint64(npieces))
	}
	// Even cut short, the frames that did go out are whole segments; the
	// stack sees the packet as sent and retransmits the rest on its own.
	return 1, nil
}

// copyPacket writes pkt, headers and all, into frame and returns the frame
// length. A packet's views begin with reserved header space that was not
// used, which is what the offset from AsViewList skips. Short frames are
// padded to the Ethernet minimum so the driver never sees a runt.
func copyPacket(frame []byte, pkt *stack.PacketBuffer) int {
	views, off := pkt.AsViewList()
	n := 0
	for v := views.Front(); v != nil; v = v.Next() {
		s := v.AsSlice()
		if off >= len(s) {
			off -= len(s)
			continue
		}
		s = s[off:]
		off = 0
		n += copy(frame[n:], s)
	}
	if n < minFrameLen {
		clear(frame[n:minFrameLen])
		n = minFrameLen
	}
	return n
}

// DebugTx describes queue i's transmit state, for tests.
func (e *Endpoint) DebugTx(i int) string {
	q, xsk := &e.txq[i], e.socks[i]
	q.mu.Lock()
	defer q.mu.Unlock()
	return fmt.Sprintf("pending %d urgent %d refused %v inflight %d freeSlots %d freeFrames %d completed %d kickPending %v",
		len(q.pending), len(q.urgent), q.refused.Load(), xsk.NumTransmitted(), xsk.NumFreeTxSlots(), xsk.FreeTxFrames(), xsk.NumCompleted(), q.kickPending.Load())
}
