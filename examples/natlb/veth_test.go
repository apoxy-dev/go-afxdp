//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/atoonk/go-afxdp"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
)

// End to end over a real veth pair and a real AF_XDP socket: a client SYN goes
// in one end, the translated packet comes out the other, the backend's reply
// goes back in, and the client sees an answer from the VIP.
//
// Both ends of the pair live in this namespace, so "the network" is the veth
// itself: what natlb transmits on the load balancer side arrives on the peer
// side, where the test plays both client and backend. That is enough to prove
// the datapath, and it leaves out the parts a namespace topology would only
// make slower to run -- see demo.sh for the version with real TCP.

// vethPair creates a connected pair, brings both ends up, and returns their
// names and hardware addresses.
func vethPair(t *testing.T) (lb, peer string, lbMAC, peerMAC [6]byte) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to create veth devices")
	}
	suffix := fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	lb, peer = "nlb"+suffix, "nlp"+suffix
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: lb}, PeerName: peer}
	if err := netlink.LinkAdd(v); err != nil {
		t.Skipf("cannot create veth pair (need CAP_NET_ADMIN): %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(v) })

	var macs [2][6]byte
	for i, name := range []string{lb, peer} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("look up %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
		ifi, err := net.InterfaceByName(name)
		if err != nil || len(ifi.HardwareAddr) != 6 {
			t.Fatalf("no MAC on %s: %v", name, err)
		}
		copy(macs[i][:], ifi.HardwareAddr)
	}
	return lb, peer, macs[0], macs[1]
}

// recvTCP drains xsk until an IPv4/TCP frame arrives or the deadline passes.
// A live link also carries kernel traffic (IPv6 neighbour discovery and the
// like), which MatchAll redirects along with ours, so anything that is not
// IPv4/TCP is skipped rather than failing the test.
func recvTCP(t *testing.T, xsk *afxdp.Socket, deadline time.Time) []byte {
	t.Helper()
	for time.Now().Before(deadline) {
		xsk.Fill(xsk.NumFreeFillSlots())
		n, err := xsk.Poll(200 * time.Millisecond)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		rx := xsk.Receive(n)
		for _, d := range rx {
			f := xsk.GetFrame(d)
			if _, perr := parse(f); perr == parseOK {
				out := append([]byte(nil), f...)
				xsk.Recycle(rx)
				return out
			}
		}
		xsk.Recycle(rx)
	}
	return nil
}

func TestVethTranslatesBothDirections(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Skipf("cannot raise memlock: %v", err)
	}
	lbIf, peerIf, lbMAC, peerMAC := vethPair(t)

	// The load balancer, on its side of the pair. Generic mode: veth is not a
	// driver with a native XDP fast path, and the test cares about the
	// translation rather than the speed.
	lbFleet, err := afxdp.Open(lbIf,
		afxdp.WithGenericMode(),
		afxdp.WithFilter(
			afxdp.MatchTCPPort(80, 443),
			afxdp.MatchTCPSrcPort(80, 443),
		),
		afxdp.WithTxReuseRxFrames(),
	)
	if err != nil {
		t.Skipf("open on %s: %v", lbIf, err)
	}
	defer lbFleet.Close()

	// The other end plays client and backend: it injects packets and reads
	// back whatever the load balancer transmits.
	peerFleet, err := afxdp.Open(peerIf,
		afxdp.WithGenericMode(),
		afxdp.WithFilter(afxdp.MatchAll()),
	)
	if err != nil {
		t.Skipf("open on %s: %v", peerIf, err)
	}
	defer peerFleet.Close()

	if !lbFleet.WaitLinkUp(5 * time.Second) {
		t.Fatal("veth did not come up")
	}

	// The backend's next hop is the peer interface, so a translated packet
	// comes back to us on the other end of the pair.
	table := NewTable(natIP, 20000, 20009)
	table.AddBackend(be1, peerMAC)

	var c counters
	lb := &balancer{
		table: table, vip: netip.AddrPortFrom(vipIP, 443), natIP: natIP,
		lbMAC: lbMAC, portLo: 20000, portHi: 20009, c: &c,
	}
	for _, xsk := range lbFleet.Sockets() {
		go lb.run(xsk)
	}
	time.Sleep(200 * time.Millisecond) // let the loops reach their first poll

	peer := peerFleet.Sockets()[0]
	deadline := time.Now().Add(10 * time.Second)

	// --- client -> VIP -----------------------------------------------------
	syn := tcpFrame(peerMAC, lbMAC, client, vipIP, 5678, 443, flagSYN, nil)
	if n, err := peer.SendBatch([][]byte{syn}); err != nil || n != 1 {
		t.Fatalf("inject SYN: sent %d, err %v", n, err)
	}

	got := recvTCP(t, peer, deadline)
	if got == nil {
		t.Fatal("nothing came back from the load balancer for the client SYN")
	}
	p, perr := parse(got)
	if perr != parseOK {
		t.Fatalf("translated frame does not parse: %v", perr)
	}
	if p.src != natIP || p.dst != be1.Addr() {
		t.Errorf("forward: addresses %s -> %s, want %s -> %s", p.src, p.dst, natIP, be1.Addr())
	}
	if p.dport != 443 {
		t.Errorf("forward: destination port %d, want the backend's 443", p.dport)
	}
	if p.sport < 20000 || p.sport > 20009 {
		t.Errorf("forward: source port %d is outside the NAT range", p.sport)
	}
	if string(got[0:6]) != string(peerMAC[:]) || string(got[6:12]) != string(lbMAC[:]) {
		t.Errorf("forward: Ethernet %x -> %x, want %x -> %x", got[6:12], got[0:6], lbMAC, peerMAC)
	}
	verifyChecksums(t, got)
	natPort := p.sport

	// --- backend -> client -------------------------------------------------
	reply := tcpFrame(peerMAC, lbMAC, be1.Addr(), natIP, 443, natPort, flagSYN|flagACK, []byte("hi"))
	if n, err := peer.SendBatch([][]byte{reply}); err != nil || n != 1 {
		t.Fatalf("inject reply: sent %d, err %v", n, err)
	}

	got = recvTCP(t, peer, deadline)
	if got == nil {
		t.Fatal("nothing came back for the backend's reply")
	}
	if p, perr = parse(got); perr != parseOK {
		t.Fatalf("reverse frame does not parse: %v", perr)
	}
	if p.src != vipIP || p.dst != client {
		t.Errorf("reverse: addresses %s -> %s, want %s -> %s", p.src, p.dst, vipIP, client)
	}
	if p.sport != 443 || p.dport != 5678 {
		t.Errorf("reverse: ports %d -> %d, want 443 -> 5678", p.sport, p.dport)
	}
	if string(got[0:6]) != string(peerMAC[:]) {
		t.Errorf("reverse: goes to %x, want the client's next hop %x", got[0:6], peerMAC)
	}
	verifyChecksums(t, got)

	// The client must see the VIP, not the backend: that is the entire point.
	if binary.BigEndian.Uint16(got[etherType:]) != etherIPv4 {
		t.Error("reverse: not IPv4 on the wire")
	}
	if table.Len() != 1 {
		t.Errorf("%d flows in the table, want the one connection", table.Len())
	}
}
