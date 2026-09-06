//go:build linux && gvisorfork

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// TestVethInlineReconnect exercises the fork's inline TCP processing across a
// connection close: the first connection's teardown runs on the receive
// goroutine, and the second connection must still be accepted afterwards.
func TestVethInlineReconnect(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	for _, tool := range []string{"ip", "ethtool"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Skipf("memlock: %v", err)
	}
	tcp.InlineProcessing = true
	defer func() { tcp.InlineProcessing = false }()

	suffix := fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	host, peer, ns := "nih"+suffix, "nip"+suffix, "ni"+suffix
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(v); err != nil {
		t.Skipf("veth: %v", err)
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

	rt, err := startNetstack(host, 0, netip.MustParsePrefix("10.98.0.1/24"), 8080, 1500, xdplink.TxFIFO, true, true, true, nil, afxdp.WithGenericMode())
	if err != nil {
		t.Fatalf("startNetstack: %v", err)
	}
	var srv server
	defer func() { srv.stopping.Store(true); rt.stop() }()
	go srv.serve(rt.ln, "echo")

	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for round := 1; round <= 3; round++ {
		c := dialInNetns(t, h, "10.98.0.1:8080")
		msg := []byte(strings.Repeat("x", 100000))
		werr := make(chan error, 1)
		go func() { _, err := c.Write(msg); werr <- err }()
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("round %d read: %v", round, err)
		}
		if err := <-werr; err != nil {
			t.Fatalf("round %d write: %v", round, err)
		}
		c.Close()
		time.Sleep(100 * time.Millisecond)
		t.Logf("round %d ok; accepted %d closed %d", round, srv.accepted.Load(), srv.closed.Load())
	}
}

// TestVethRTC runs the run-to-completion echo over a veth with inline TCP
// processing: several connections, a bulk transfer on each, clean closes.
func TestVethRTC(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	for _, tool := range []string{"ip", "ethtool"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Skipf("memlock: %v", err)
	}
	tcp.InlineProcessing = true
	defer func() { tcp.InlineProcessing = false }()

	suffix := fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	host, peer, ns := "nrh"+suffix, "nrp"+suffix, "nr"+suffix
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(v); err != nil {
		t.Skipf("veth: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(v) })
	run(t, "ip", "netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	run(t, "ip", "link", "set", peer, "netns", ns)
	run(t, "ip", "-n", ns, "addr", "add", "10.97.0.2/24", "dev", peer)
	run(t, "ip", "-n", ns, "link", "set", "lo", "up")
	run(t, "ip", "-n", ns, "link", "set", peer, "up")
	run(t, "ip", "netns", "exec", ns, "ethtool", "-K", peer, "tx", "off", "tso", "off", "gso", "off")
	run(t, "ip", "addr", "add", "10.97.0.1/24", "dev", host)
	run(t, "ip", "link", "set", host, "up")

	var srv server
	rt, err := startNetstack(host, 0, netip.MustParsePrefix("10.97.0.1/24"), 8080, 1500, xdplink.TxDirect, true, true, true,
		func() *rtcServer { return newRTCServer(&srv) }, afxdp.WithGenericMode())
	if err != nil {
		t.Fatalf("startNetstack: %v", err)
	}
	defer func() { srv.stopping.Store(true); rt.stop() }()

	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for round := 1; round <= 3; round++ {
		c := dialInNetns(t, h, "10.97.0.1:8080")
		msg := make([]byte, 300000)
		for i := range msg {
			msg[i] = byte(i*31 + round)
		}
		werr := make(chan error, 1)
		go func() { _, err := c.Write(msg); werr <- err }()
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("round %d read: %v", round, err)
		}
		if err := <-werr; err != nil {
			t.Fatalf("round %d write: %v", round, err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatalf("round %d: echo differs", round)
		}
		c.Close()
		time.Sleep(100 * time.Millisecond)
		t.Logf("round %d ok; accepted %d closed %d", round, srv.accepted.Load(), srv.closed.Load())
	}
}
