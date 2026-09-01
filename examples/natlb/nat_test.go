//go:build linux

package main

import (
	"encoding/binary"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"
)

var (
	client  = netip.MustParseAddr("1.2.3.4")
	vipIP   = netip.MustParseAddr("203.0.113.10")
	natIP   = netip.MustParseAddr("10.0.0.10")
	be1     = netip.MustParseAddrPort("10.0.0.21:443")
	be2     = netip.MustParseAddrPort("10.0.0.22:443")
	macCli  = [6]byte{0x02, 0, 0, 0, 0, 0x01}
	macLB   = [6]byte{0x02, 0, 0, 0, 0, 0x10}
	macBE1  = [6]byte{0x02, 0, 0, 0, 0, 0x21}
	macBE2  = [6]byte{0x02, 0, 0, 0, 0, 0x22}
	nowFake = time.Unix(1700000000, 0)
)

// tcpFrame builds an Ethernet+IPv4+TCP packet with a correct checksum, so a
// test can assert that translation keeps it correct rather than merely
// changing it.
func tcpFrame(smac, dmac [6]byte, src, dst netip.Addr, sport, dport uint16, flags uint8, payload []byte) []byte {
	const ipLen, tcpLen = 20, 20
	f := make([]byte, etherLen+ipLen+tcpLen+len(payload))
	copy(f[0:6], dmac[:])
	copy(f[6:12], smac[:])
	binary.BigEndian.PutUint16(f[12:], etherIPv4)

	ip := f[etherLen:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen+tcpLen+len(payload)))
	ip[8] = 64
	ip[9] = protoTCP
	s4, d4 := src.As4(), dst.As4()
	copy(ip[12:16], s4[:])
	copy(ip[16:20], d4[:])

	tcp := f[etherLen+ipLen:]
	binary.BigEndian.PutUint16(tcp[0:], sport)
	binary.BigEndian.PutUint16(tcp[2:], dport)
	tcp[12] = 5 << 4 // data offset: 20 bytes
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:], 0xffff)
	copy(tcp[tcpLen:], payload)

	ipChecksum(f, etherLen)
	tcpChecksum(f, etherLen, etherLen+ipLen, etherLen+ipLen+tcpLen+len(payload))
	return f
}

// verifyChecksums sums each header the way a receiver does: over a correct
// packet the result is zero. This is deliberately not the code under test --
// it checks the answer instead of repeating the calculation.
func verifyChecksums(t *testing.T, f []byte) {
	t.Helper()
	p, err := parse(f)
	if err != parseOK {
		t.Fatalf("verify: unparseable frame (%v)", err)
	}
	ihl := int(f[p.l3]&0x0f) * 4
	if got := onesComplement(sum(f[p.l3:p.l3+ihl], 0)); got != 0 {
		t.Errorf("IPv4 checksum does not verify: residue %#04x, want 0", got)
	}
	total := int(binary.BigEndian.Uint16(f[p.l3+2:]))
	seg := f[p.l4 : p.l3+total]
	s := sum(f[p.l3+12:p.l3+20], 0)
	s += uint32(protoTCP) + uint32(len(seg))
	if got := onesComplement(sum(seg, s)); got != 0 {
		t.Errorf("TCP checksum does not verify: residue %#04x, want 0", got)
	}
}

func newTestTable() *Table {
	t := NewTable(natIP, 20000, 20009)
	t.AddBackend(be1, macBE1)
	return t
}

func TestParseGoodFrame(t *testing.T) {
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, []byte("hello"))
	p, err := parse(f)
	if err != parseOK {
		t.Fatalf("parse: %v", err)
	}
	if p.src != client || p.dst != vipIP || p.sport != 5678 || p.dport != 443 {
		t.Errorf("tuple is %s:%d -> %s:%d", p.src, p.sport, p.dst, p.dport)
	}
	if p.l3 != 14 || p.l4 != 34 {
		t.Errorf("offsets l3=%d l4=%d, want 14 and 34", p.l3, p.l4)
	}
	if p.flags&flagSYN == 0 {
		t.Error("SYN flag was lost")
	}
}

// The TCP header is found through the IHL field, not at a fixed offset. A
// packet with IP options would otherwise have its ports read out of the
// middle of the IP header.
func TestParseHonoursIHL(t *testing.T) {
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	// Splice four bytes of IP option in and say so in IHL.
	withOpt := make([]byte, 0, len(f)+4)
	withOpt = append(withOpt, f[:etherLen+20]...)
	withOpt = append(withOpt, 1, 1, 1, 1) // four no-ops
	withOpt = append(withOpt, f[etherLen+20:]...)
	withOpt[etherLen] = 0x46 // IHL = 6 words
	binary.BigEndian.PutUint16(withOpt[etherLen+2:], uint16(24+20))

	p, err := parse(withOpt)
	if err != parseOK {
		t.Fatalf("parse: %v", err)
	}
	if p.l4 != etherLen+24 {
		t.Errorf("l4 = %d, want %d: the options were not skipped", p.l4, etherLen+24)
	}
	if p.dport != 443 {
		t.Errorf("dport = %d, want 443: ports read from the wrong offset", p.dport)
	}
}

func TestParseRejections(t *testing.T) {
	good := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)

	t.Run("truncated", func(t *testing.T) {
		for n := 0; n < len(good); n += 7 {
			if _, err := parse(good[:n]); err == parseOK {
				t.Fatalf("a %d-byte frame parsed as a whole packet", n)
			}
		}
	})
	t.Run("not IPv4", func(t *testing.T) {
		f := append([]byte(nil), good...)
		binary.BigEndian.PutUint16(f[12:], 0x86dd) // IPv6
		if _, err := parse(f); err != parseNotIPv4 {
			t.Errorf("got %v, want parseNotIPv4", err)
		}
	})
	t.Run("not TCP", func(t *testing.T) {
		f := append([]byte(nil), good...)
		f[etherLen+9] = 17 // UDP
		if _, err := parse(f); err != parseNotTCP {
			t.Errorf("got %v, want parseNotTCP", err)
		}
	})
	t.Run("fragment offset", func(t *testing.T) {
		f := append([]byte(nil), good...)
		binary.BigEndian.PutUint16(f[etherLen+6:], 100)
		if _, err := parse(f); err != parseFragment {
			t.Errorf("got %v, want parseFragment", err)
		}
	})
	t.Run("more fragments", func(t *testing.T) {
		f := append([]byte(nil), good...)
		binary.BigEndian.PutUint16(f[etherLen+6:], 0x2000)
		if _, err := parse(f); err != parseFragment {
			t.Errorf("got %v, want parseFragment", err)
		}
	})
	t.Run("IHL below the minimum", func(t *testing.T) {
		f := append([]byte(nil), good...)
		f[etherLen] = 0x43 // IHL 3 words: shorter than an IPv4 header can be
		if _, err := parse(f); err != parseTooShort {
			t.Errorf("got %v, want parseTooShort", err)
		}
	})
}

func TestForwardRewrite(t *testing.T) {
	tab := newTestTable()
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, []byte("GET /"))
	p, _ := parse(f)

	fl, ok := tab.Forward(p, macCli, nowFake)
	if !ok {
		t.Fatal("no flow was created for a SYN to the VIP")
	}
	rewrite(f, p, macLB, fl.backend.MAC, natIP, fl.backend.Addr.Addr(), fl.natPort, fl.backend.Addr.Port())

	got, err := parse(f)
	if err != parseOK {
		t.Fatalf("rewritten frame does not parse: %v", err)
	}
	if got.src != natIP || got.dst != be1.Addr() {
		t.Errorf("addresses are %s -> %s, want %s -> %s", got.src, got.dst, natIP, be1.Addr())
	}
	if got.sport != fl.natPort || got.dport != 443 {
		t.Errorf("ports are %d -> %d, want %d -> 443", got.sport, got.dport, fl.natPort)
	}
	if fl.natPort == 5678 {
		t.Error("the client's source port was kept; two clients on the same port would collide at the backend")
	}
	if string(f[0:6]) != string(macBE1[:]) || string(f[6:12]) != string(macLB[:]) {
		t.Errorf("Ethernet addresses are %x -> %x", f[6:12], f[0:6])
	}
	verifyChecksums(t, f)
}

func TestReverseRewrite(t *testing.T) {
	tab := newTestTable()
	out := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p, _ := parse(out)
	fl, _ := tab.Forward(p, macCli, nowFake)

	// The backend answers the translated tuple.
	reply := tcpFrame(macBE1, macLB, be1.Addr(), natIP, 443, fl.natPort, flagSYN|flagACK, []byte("200 OK"))
	rp, _ := parse(reply)
	rfl, ok := tab.Reverse(rp, nowFake)
	if !ok {
		t.Fatal("the reply did not find its flow")
	}
	if rfl != fl {
		t.Fatal("the reverse lookup found a different flow")
	}
	rewrite(reply, rp, macLB, rfl.clientMAC, rfl.fwd.dst, rfl.fwd.src, rfl.fwd.dport, rfl.fwd.sport)

	got, _ := parse(reply)
	if got.src != vipIP || got.dst != client {
		t.Errorf("addresses are %s -> %s, want %s -> %s", got.src, got.dst, vipIP, client)
	}
	if got.sport != 443 || got.dport != 5678 {
		t.Errorf("ports are %d -> %d, want 443 -> 5678", got.sport, got.dport)
	}
	if string(reply[0:6]) != string(macCli[:]) {
		t.Errorf("reply goes to %x, want the client's next hop %x", reply[0:6], macCli)
	}
	verifyChecksums(t, reply)
}

// A translated packet must still verify at every size, including one small
// enough that the frame carries Ethernet padding the checksum must not cover.
func TestChecksumsAcrossSizes(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 5, 46, 100, 1400} {
		tab := newTestTable()
		f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, make([]byte, n))
		if n < 46 {
			f = append(f, make([]byte, 46-n)...) // pad to the Ethernet minimum
		}
		p, _ := parse(f)
		fl, ok := tab.Forward(p, macCli, nowFake)
		if !ok {
			t.Fatalf("payload %d: no flow", n)
		}
		rewrite(f, p, macLB, fl.backend.MAC, natIP, fl.backend.Addr.Addr(), fl.natPort, fl.backend.Addr.Port())
		t.Run("payload"+strconv.Itoa(n), func(t *testing.T) { verifyChecksums(t, f) })
	}
}

func TestFlowKeysAreDirectional(t *testing.T) {
	tab := newTestTable()
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p, _ := parse(f)
	fl, _ := tab.Forward(p, macCli, nowFake)

	if fl.fwd.src != client || fl.fwd.dst != vipIP || fl.fwd.dport != 443 {
		t.Errorf("forward key is %+v", fl.fwd)
	}
	if fl.rev.src != be1.Addr() || fl.rev.dst != natIP || fl.rev.dport != fl.natPort {
		t.Errorf("reverse key is %+v", fl.rev)
	}
	if fl.rev.sport != 443 {
		t.Errorf("reverse source port is %d, want the backend's 443", fl.rev.sport)
	}
}

// Two clients that happen to pick the same source port must not collide at the
// backend, which is the whole reason for SNAT rather than keeping the port.
func TestSameClientPortGetsDistinctNATPorts(t *testing.T) {
	tab := newTestTable()
	other := netip.MustParseAddr("5.6.7.8")

	f1 := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p1, _ := parse(f1)
	fl1, _ := tab.Forward(p1, macCli, nowFake)

	f2 := tcpFrame(macCli, macLB, other, vipIP, 5678, 443, flagSYN, nil)
	p2, _ := parse(f2)
	fl2, ok := tab.Forward(p2, macCli, nowFake)
	if !ok {
		t.Fatal("the second client got no flow")
	}
	if fl1.natPort == fl2.natPort {
		t.Fatalf("both clients were translated onto port %d", fl1.natPort)
	}
}

func TestPortPoolAllocateReleaseExhaust(t *testing.T) {
	p := newPortPool(100, 102)
	var got []uint16
	for i := 0; i < 3; i++ {
		port, ok := p.get()
		if !ok {
			t.Fatalf("pool ran dry after %d of 3", i)
		}
		got = append(got, port)
	}
	if _, ok := p.get(); ok {
		t.Fatal("a fourth port came out of a pool of three")
	}
	if p.avail() != 0 {
		t.Errorf("avail says %d with none left", p.avail())
	}
	p.put(got[0])
	if p.avail() != 1 {
		t.Errorf("avail says %d after one was returned", p.avail())
	}
	if port, ok := p.get(); !ok || port != got[0] {
		t.Errorf("got %d/%v after returning %d", port, ok, got[0])
	}
}

// Exhaustion must be refused cleanly and counted, not papered over by reusing
// a port that is still in use.
func TestNATPortExhaustionIsRefused(t *testing.T) {
	tab := NewTable(natIP, 20000, 20001) // exactly two
	tab.AddBackend(be1, macBE1)
	for i := 0; i < 2; i++ {
		f := tcpFrame(macCli, macLB, client, vipIP, uint16(5000+i), 443, flagSYN, nil)
		p, _ := parse(f)
		if _, ok := tab.Forward(p, macCli, nowFake); !ok {
			t.Fatalf("flow %d was refused with ports still free", i)
		}
	}
	f := tcpFrame(macCli, macLB, client, vipIP, 5999, 443, flagSYN, nil)
	p, _ := parse(f)
	if _, ok := tab.Forward(p, macCli, nowFake); ok {
		t.Fatal("a third flow was created from a pool of two ports")
	}
	if got := tab.Stats().NoPort; got != 1 {
		t.Errorf("port exhaustion counter is %d, want 1", got)
	}
}

func TestBackendAddRemove(t *testing.T) {
	tab := newTestTable()
	if got := len(tab.Backends()); got != 1 {
		t.Fatalf("%d backends after one add", got)
	}
	tab.AddBackend(be1, macBE1) // again
	if got := len(tab.Backends()); got != 1 {
		t.Errorf("adding the same backend twice gave %d entries", got)
	}
	tab.AddBackend(be2, macBE2)
	if got := len(tab.Backends()); got != 2 {
		t.Errorf("%d backends after adding a second", got)
	}
	if !tab.RemoveBackend(be1) {
		t.Error("removing a present backend said it was absent")
	}
	if tab.RemoveBackend(be1) {
		t.Error("removing it twice said it was present")
	}
	if got := tab.Backends(); len(got) != 1 || got[0].Addr != be2 {
		t.Errorf("left with %v, want only %s", got, be2)
	}
}

// New flows only go to backends in rotation, and flows already placed stay
// where they are: removing a backend drains it, it does not reset it.
func TestRemovedBackendKeepsItsFlows(t *testing.T) {
	tab := newTestTable()
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p, _ := parse(f)
	fl, _ := tab.Forward(p, macCli, nowFake)
	if fl.backend.Addr != be1 {
		t.Fatalf("landed on %s", fl.backend.Addr)
	}
	tab.RemoveBackend(be1)

	// The established flow still resolves, to the same backend.
	reply := tcpFrame(macBE1, macLB, be1.Addr(), natIP, 443, fl.natPort, flagACK, nil)
	rp, _ := parse(reply)
	if got, ok := tab.Reverse(rp, nowFake); !ok || got.backend.Addr != be1 {
		t.Error("an established flow lost its backend when the backend was removed")
	}
	// A new one has nowhere to go.
	f2 := tcpFrame(macCli, macLB, client, vipIP, 6000, 443, flagSYN, nil)
	p2, _ := parse(f2)
	if _, ok := tab.Forward(p2, macCli, nowFake); ok {
		t.Error("a new flow was placed on an empty rotation")
	}
}

func TestSelectionSpreadsAcrossBackends(t *testing.T) {
	tab := NewTable(natIP, 20000, 20999) // room for every flow below
	tab.AddBackend(be1, macBE1)
	tab.AddBackend(be2, macBE2)
	seen := map[netip.AddrPort]int{}
	for i := 0; i < 200; i++ {
		f := tcpFrame(macCli, macLB, client, vipIP, uint16(5000+i), 443, flagSYN, nil)
		p, _ := parse(f)
		fl, ok := tab.Forward(p, macCli, nowFake)
		if !ok {
			t.Fatalf("flow %d refused", i)
		}
		seen[fl.backend.Addr]++
	}
	if len(seen) != 2 {
		t.Fatalf("only %d backend(s) were used: %v", len(seen), seen)
	}
	// Power of two choices on equal backends should be close to even; this is
	// a sanity bound, not a distribution test.
	a, b := seen[be1], seen[be2]
	if a < 60 || b < 60 {
		t.Errorf("badly skewed: %s=%d %s=%d", be1, a, be2, b)
	}
}

// A mid-connection packet for a flow we have no state for must not invent one:
// it would be sent to an arbitrary backend that has never seen the handshake.
func TestNonSYNWithoutStateIsRefused(t *testing.T) {
	tab := newTestTable()
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagACK, []byte("data"))
	p, _ := parse(f)
	if _, ok := tab.Forward(p, macCli, nowFake); ok {
		t.Error("a bare ACK created a flow")
	}
}

func TestReplyWithoutStateIsRefused(t *testing.T) {
	tab := newTestTable()
	reply := tcpFrame(macBE1, macLB, be1.Addr(), natIP, 443, 20005, flagACK, nil)
	p, _ := parse(reply)
	if _, ok := tab.Reverse(p, nowFake); ok {
		t.Error("a reply on an unallocated NAT port found state")
	}
}

// A reset ends the flow, but not while the reset itself is still being sent.
//
// The packet that kills the flow is translated onto that flow's NAT port, so
// returning the port to the pool during the lookup lets the very next flow
// allocate it while the reset is in flight -- and the reset then lands on
// somebody else's connection. Under port pressure the FIFO gives no cushion at
// all: the freed port goes to the head of an empty queue. So the flow is
// marked and reaped by the next Expire instead.
func TestResetKeepsItsPortUntilTheNextSweep(t *testing.T) {
	tab := newTestTable()
	free := tab.ports.avail()
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p, _ := parse(f)
	fl, _ := tab.Forward(p, macCli, nowFake)

	rst := tcpFrame(macBE1, macLB, be1.Addr(), natIP, 443, fl.natPort, flagRST, nil)
	rp, _ := parse(rst)
	got, ok := tab.Reverse(rp, nowFake)
	if !ok || got != fl {
		t.Fatal("the reset was not translated; the other end would never learn the flow died")
	}
	if tab.ports.avail() != free-1 {
		t.Errorf("the NAT port went back to the pool while the reset still carries it: %d free, want %d",
			tab.ports.avail(), free-1)
	}

	// The next sweep reaps it, whatever the clock says.
	if n := tab.Expire(nowFake, time.Minute, time.Second); n != 1 {
		t.Errorf("the sweep reaped %d flows, want the reset one", n)
	}
	if tab.Len() != 0 {
		t.Errorf("%d flows survive a reset", tab.Len())
	}
	if tab.ports.avail() != free {
		t.Errorf("NAT port was not returned: %d free, want %d", tab.ports.avail(), free)
	}
	if n := tab.Backends()[0].Active; n != 0 {
		t.Errorf("backend still counts %d flows", n)
	}
	if st := tab.Stats(); st.Reset != 1 {
		t.Errorf("reset counter is %d, want 1", st.Reset)
	}
}

// The bug this guards: exhaust the pool, reset a flow you control, and the
// freed port is handed to the next flow while your reset still carries it.
func TestResetUnderPortPressureDoesNotHandOffItsPort(t *testing.T) {
	tab := NewTable(natIP, 20000, 20001) // two ports: pressure by construction
	tab.AddBackend(be1, macBE1)

	var flows []*flow
	for i := 0; i < 2; i++ {
		f := tcpFrame(macCli, macLB, client, vipIP, uint16(5000+i), 443, flagSYN, nil)
		p, _ := parse(f)
		fl, ok := tab.Forward(p, macCli, nowFake)
		if !ok {
			t.Fatalf("flow %d refused", i)
		}
		flows = append(flows, fl)
	}
	victim := flows[0]

	rst := tcpFrame(macBE1, macLB, be1.Addr(), natIP, 443, victim.natPort, flagRST, nil)
	rp, _ := parse(rst)
	if _, ok := tab.Reverse(rp, nowFake); !ok {
		t.Fatal("the reset was not translated")
	}

	// A new flow arriving before the sweep must not be given the port the
	// in-flight reset is stamped with.
	f := tcpFrame(macCli, macLB, client, vipIP, 6000, 443, flagSYN, nil)
	p, _ := parse(f)
	if fresh, ok := tab.Forward(p, macCli, nowFake); ok && fresh.natPort == victim.natPort {
		t.Fatalf("new flow got NAT port %d, the one the outgoing reset still carries: "+
			"that reset would tear down this connection instead", fresh.natPort)
	}
}

func TestIdleAndDrainExpiry(t *testing.T) {
	const idle, drain = 2 * time.Minute, 5 * time.Second

	t.Run("idle flow survives until the timeout", func(t *testing.T) {
		tab := newTestTable()
		f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
		p, _ := parse(f)
		tab.Forward(p, macCli, nowFake)

		if n := tab.Expire(nowFake.Add(idle-time.Second), idle, drain); n != 0 {
			t.Errorf("%d flows expired early", n)
		}
		if n := tab.Expire(nowFake.Add(idle), idle, drain); n != 1 {
			t.Errorf("%d flows expired at the timeout, want 1", n)
		}
		if tab.Len() != 0 {
			t.Errorf("%d flows left", tab.Len())
		}
	})

	t.Run("closing flow uses the shorter drain", func(t *testing.T) {
		tab := newTestTable()
		f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
		p, _ := parse(f)
		fl, _ := tab.Forward(p, macCli, nowFake)

		fin := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagFIN|flagACK, nil)
		fp, _ := parse(fin)
		tab.Forward(fp, macCli, nowFake)

		bfin := tcpFrame(macBE1, macLB, be1.Addr(), natIP, 443, fl.natPort, flagFIN|flagACK, nil)
		bp, _ := parse(bfin)
		tab.Reverse(bp, nowFake)

		if n := tab.Expire(nowFake.Add(drain), idle, drain); n != 1 {
			t.Errorf("%d closed flows expired after the drain, want 1", n)
		}
	})

	t.Run("ports come back", func(t *testing.T) {
		tab := newTestTable()
		free := tab.ports.avail()
		for i := 0; i < 5; i++ {
			f := tcpFrame(macCli, macLB, client, vipIP, uint16(5000+i), 443, flagSYN, nil)
			p, _ := parse(f)
			tab.Forward(p, macCli, nowFake)
		}
		if tab.ports.avail() != free-5 {
			t.Fatalf("%d free after 5 flows, want %d", tab.ports.avail(), free-5)
		}
		tab.Expire(nowFake.Add(time.Hour), idle, drain)
		if tab.ports.avail() != free {
			t.Errorf("%d free after expiry, want all %d back", tab.ports.avail(), free)
		}
	})
}

// A packet keeps a flow alive; a flow being touched must not be expired.
func TestTrafficRefreshesTheFlow(t *testing.T) {
	const idle, drain = time.Minute, 5 * time.Second
	tab := newTestTable()
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p, _ := parse(f)
	tab.Forward(p, macCli, nowFake)

	later := nowFake.Add(idle - time.Second)
	data := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagACK, []byte("x"))
	dp, _ := parse(data)
	if _, ok := tab.Forward(dp, macCli, later); !ok {
		t.Fatal("an established flow was not found")
	}
	if n := tab.Expire(later.Add(idle-time.Second), idle, drain); n != 0 {
		t.Errorf("%d flows expired despite recent traffic", n)
	}
}

// A backend that goes away and comes back must be the SAME backend, still
// carrying its flows.
//
// Removing it and adding it again used to allocate a second object with a flow
// count of zero, while every established connection still pointed at the
// first. Selection then saw an idle server and sent new flows to whichever
// backend was actually the busiest. Health checks flap, so this is the
// ordinary case rather than a corner.
func TestBackendFlapKeepsItsFlowCount(t *testing.T) {
	tab := NewTable(natIP, 20000, 20999)
	tab.AddBackend(be1, macBE1)
	tab.AddBackend(be2, macBE2)

	for i := 0; i < 400; i++ {
		f := tcpFrame(macCli, macLB, client, vipIP, uint16(5000+i), 443, flagSYN, nil)
		p, _ := parse(f)
		if _, ok := tab.Forward(p, macCli, nowFake); !ok {
			t.Fatalf("flow %d refused", i)
		}
	}
	total := func() int {
		n := 0
		for _, b := range tab.Backends() {
			n += b.Active
		}
		return n
	}
	if total() != 400 || tab.Len() != 400 {
		t.Fatalf("before the flap: backends report %d, table holds %d", total(), tab.Len())
	}

	tab.RemoveBackend(be1)
	tab.AddBackend(be1, macBE1)

	if got := total(); got != tab.Len() {
		t.Errorf("after one flap the backends report %d flows but the table holds %d: "+
			"selection is now working from a false picture", got, tab.Len())
	}
	for _, b := range tab.Backends() {
		if b.Addr == be1 && b.Active == 0 {
			t.Error("the flapped backend reports no flows while it is still carrying them")
		}
	}
	if n := len(tab.Backends()); n != 2 {
		t.Errorf("%d backends after a flap, want 2", n)
	}
}

// A drained backend keeps its flows but takes no new ones, and disappears once
// the last one has gone.
func TestDrainedBackendLeavesOnceEmpty(t *testing.T) {
	tab := NewTable(natIP, 20000, 20999)
	tab.AddBackend(be1, macBE1)
	tab.AddBackend(be2, macBE2)

	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	p, _ := parse(f)
	fl, _ := tab.Forward(p, macCli, nowFake)
	pinned := fl.backend.Addr

	tab.RemoveBackend(pinned)
	for _, b := range tab.Backends() {
		if b.Addr == pinned && !b.Drained {
			t.Error("the removed backend is not marked draining")
		}
	}
	// New flows avoid it entirely.
	for i := 0; i < 100; i++ {
		f := tcpFrame(macCli, macLB, client, vipIP, uint16(6000+i), 443, flagSYN, nil)
		p, _ := parse(f)
		nf, ok := tab.Forward(p, macCli, nowFake)
		if !ok {
			t.Fatalf("flow %d refused", i)
		}
		if nf.backend.Addr == pinned {
			t.Fatalf("a new flow landed on the draining backend %s", pinned)
		}
	}
	// Once its last flow goes, so does it.
	tab.Expire(nowFake.Add(time.Hour), time.Minute, time.Second)
	for _, b := range tab.Backends() {
		if b.Addr == pinned {
			t.Errorf("drained backend %s is still listed with %d flows", b.Addr, b.Active)
		}
	}
}

// A packet whose IPv4 total length does not describe a real TCP segment must be
// refused by parse. It used to be accepted, rewritten, and then transmitted
// with the ORIGINAL checksum, because tcpChecksum quietly declined to compute
// one for a segment it thought was too short.
func TestBogusTotalLengthIsRefused(t *testing.T) {
	for _, total := range []int{0, 1, 19, 20, 21, 39, 1500, 65535} {
		f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
		binary.BigEndian.PutUint16(f[etherLen+2:], uint16(total))
		if _, err := parse(f); err == parseOK {
			t.Errorf("total length %d was accepted; the frame would go out rewritten "+
				"but not re-checksummed", total)
		}
	}
	// The honest value still works, padding and all.
	f := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)
	f = append(f, make([]byte, 6)...) // Ethernet padding past the IP total length
	if _, err := parse(f); err != parseOK {
		t.Errorf("a correct frame with Ethernet padding was refused: %v", err)
	}
}

// The stats path must be safe to read while the datapath runs.
//
// This is the test that was missing. The suite exercised the translation
// thoroughly but never started anything resembling report(), so a stats
// goroutine reading a live *Backend while queue goroutines incremented its
// flow count went unnoticed: -race over the whole suite came back clean while
// the shipped binary raced on its very first stats tick, at any queue count.
func TestStatsAreSafeToReadWhileTranslating(t *testing.T) {
	tab := NewTable(natIP, 20000, 20999)
	tab.AddBackend(be1, macBE1)
	tab.AddBackend(be2, macBE2)

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Two "queues" creating and expiring flows.
	for q := 0; q < 2; q++ {
		wg.Add(1)
		go func(q int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				port := uint16(1024 + (q*5000+i)%5000)
				f := tcpFrame(macCli, macLB, client, vipIP, port, 443, flagSYN, nil)
				p, _ := parse(f)
				tab.Forward(p, macCli, nowFake)
				if i%16 == 0 {
					tab.Expire(nowFake.Add(time.Hour), time.Minute, time.Second)
				}
			}
		}(q)
	}
	// And the reporter, doing exactly what report() does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			st := tab.Stats()
			_ = st.Active + st.Created + st.Expired + st.Reset + st.NoPort
			for _, b := range tab.Backends() {
				_ = b.Active
				_ = b.Drained
			}
		}
	}()
	// And health-check style churn, which is what exposed the flap bug.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			tab.RemoveBackend(be2)
			tab.AddBackend(be2, macBE2)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(done)
	wg.Wait()
}
