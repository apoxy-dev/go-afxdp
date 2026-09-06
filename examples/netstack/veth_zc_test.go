//go:build linux && gvisorfork

package main

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestVethZeroCopySource streams the cache to a kernel peer over a veth,
// checks every byte against the pattern fillCache wrote, and that the
// frames left the cache in place rather than through a copy.
func TestVethZeroCopySource(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create veth devices")
	}
	for _, tool := range []string{"ip", "ethtool"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Skipf("cannot raise memlock: %v", err)
	}
	suffix := fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	host, peer, ns := "zch"+suffix, "zcp"+suffix, "zc"+suffix
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(v); err != nil {
		t.Skipf("cannot create veth pair (need CAP_NET_ADMIN): %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(v) })
	run(t, "ip", "netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	run(t, "ip", "link", "set", peer, "netns", ns)
	run(t, "ip", "-n", ns, "addr", "add", "10.98.0.2/24", "dev", peer)
	run(t, "ip", "-n", ns, "link", "set", "lo", "up")
	run(t, "ip", "-n", ns, "link", "set", peer, "up")
	run(t, "ip", "netns", "exec", ns, "ethtool", "-K", peer, "tx", "off", "tso", "off", "gso", "off")
	run(t, "ip", "addr", "add", "10.98.0.1/24", "dev", host)
	run(t, "ip", "link", "set", host, "up")

	old1, old2 := cacheBytes, rtcSource
	cacheBytes, rtcSource = 4<<20, true
	t.Cleanup(func() { cacheBytes, rtcSource = old1, old2 })

	const port = 8080
	prefix := netip.MustParsePrefix("10.98.0.1/24")
	var srv server
	rt, err := startNetstack(host, 0, prefix, port, 1500, xdplink.TxDirect, true, true, true,
		func() *rtcServer { return newRTCServer(&srv) }, afxdp.WithGenericMode())
	if err != nil {
		t.Fatalf("startNetstack: %v", err)
	}
	defer func() { srv.stopping.Store(true); rt.stop() }()
	if rt.cache == nil {
		t.Fatal("no cache")
	}

	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatalf("netns: %v", err)
	}
	defer h.Close()
	c := dialInNetns(t, h, fmt.Sprintf("10.98.0.1:%d", port))
	defer c.Close()

	// Read a few laps of the cache and check the pattern byte by byte.
	n := cachePayload(rt.cache)
	frames := rt.cache.Frames()
	total := 3*frames*n + n/2
	buf := make([]byte, total)
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", total, err)
	}
	for k, b := range buf {
		i, j := (k/n)%frames, k%n
		if want := byte(i*7 + j); b != want {
			t.Fatalf("byte %d (frame %d, offset %d) = %#x, want %#x", k, i, j, b, want)
		}
	}
	es := rt.ep.Stats()
	if es.TxZeroCopy == 0 {
		t.Fatalf("nothing went out in place: %+v", es)
	}
	if es.TxDropped != 0 || es.UnknownMAC != 0 {
		t.Errorf("endpoint drops: %+v", es)
	}
	t.Logf("%d bytes verified; frames in place %d, copied %d, dropped %d", total, es.TxZeroCopy, es.Tx-es.TxZeroCopy, es.TxDropped)
	if n := rt.s.Stats().TCP.ChecksumErrors.Value(); n != 0 {
		t.Errorf("netstack saw %d checksum errors", n)
	}
}
