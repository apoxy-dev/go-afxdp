//go:build linux

package afxdp

import (
	"bytes"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
)

// One UMEM registered by two sockets (a two-queue veth), a frame built in the
// extra area and transmitted from a socket whose own frames it is not, then
// handed back through OnForeignComplete.
func TestVethSharedMemory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create veth devices")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Skipf("cannot raise memlock: %v", err)
	}
	suffix := fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	a, b := "sha"+suffix, "shb"+suffix
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: a, NumRxQueues: 2, NumTxQueues: 2}, PeerName: b,
		PeerNumRxQueues: 2, PeerNumTxQueues: 2}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Skipf("cannot create veth pair (need CAP_NET_ADMIN): %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(veth) })
	for _, name := range []string{a, b} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("look up %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
	}

	var completed atomic.Uint64
	var lastAddr atomic.Uint64
	const extra = 64 << 10
	tx, err := Open(a, WithGenericMode(), WithFilter(MatchNone()), WithQueues(2), WithNumFrames(256), WithRingSize(64),
		WithSharedMemory(extra), WithOnForeignComplete(func(addr uint64) { completed.Add(1); lastAddr.Store(addr) }))
	if err != nil {
		t.Fatalf("open shared-memory fleet: %v", err)
	}
	defer tx.Close()
	rx, err := Open(b, WithGenericMode(), WithFilter(MatchAll()), WithQueues(2))
	if err != nil {
		t.Fatalf("open rx fleet: %v", err)
	}
	defer rx.Close()
	if !tx.WaitLinkUp(5 * time.Second) {
		t.Fatal("veth link did not come up")
	}
	if len(tx.Sockets()) != 2 {
		t.Fatalf("got %d sockets, want 2 (the point is two registrations of one region)", len(tx.Sockets()))
	}
	s0, s1 := tx.Sockets()[0], tx.Sockets()[1]
	if &s0.UMEM()[0] != &s1.UMEM()[0] || len(s0.UMEM()) != len(tx.Memory()) {
		t.Fatal("sockets do not share the fleet's memory")
	}
	base, mem := tx.Extra()
	if mem == nil || len(mem) < extra {
		t.Fatalf("Extra() = %d bytes at %d, want %d", len(mem), base, extra)
	}

	// A frame in the extra area, sent from the second socket (whose own
	// frames sit well below the extra area).
	frame := testFrame()
	addr := base + uint64(s1.FrameSize()) // second chunk of the area, at a chunk boundary
	copy(mem[addr-base:], frame)
	desc := Desc{Addr: addr, Len: uint32(len(frame))}

	got := make(chan int, 1)
	go func() {
		n := 0
		deadline := time.Now().Add(8 * time.Second)
		for _, xsk := range rx.Sockets() {
			n += receiveCount(xsk, 1, deadline)
			if n > 0 {
				break
			}
		}
		got <- n
	}()
	if n := s1.Transmit([]Desc{desc}); n != 1 {
		t.Fatalf("Transmit queued %d, want 1", n)
	}
	// Prove the bytes came from the extra area and not a copy.
	if !bytes.Equal(s1.GetFrame(desc), frame) {
		t.Fatal("descriptor does not read back the frame")
	}
	if n := <-got; n != 1 {
		t.Fatalf("peer received %d frames, want 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for completed.Load() == 0 && time.Now().Before(deadline) {
		s1.Complete(s1.NumCompleted())
		time.Sleep(5 * time.Millisecond)
	}
	if completed.Load() != 1 || lastAddr.Load() != addr {
		t.Fatalf("foreign completions %d (addr %d), want 1 at %d; free tx frames %d of %d",
			completed.Load(), lastAddr.Load(), addr, s1.FreeTxFrames(), s1.options.TxFrames)
	}
	if s1.FreeTxFrames() != s1.options.TxFrames {
		t.Errorf("a foreign frame leaked into the transmit pool: %d free of %d", s1.FreeTxFrames(), s1.options.TxFrames)
	}
}
