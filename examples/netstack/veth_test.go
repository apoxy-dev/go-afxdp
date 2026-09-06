//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// End to end through a real kernel TCP client: a veth pair with one end moved
// into a network namespace, the netstack server on the host end over a real
// AF_XDP socket (generic mode, since veth has no zero-copy driver), and an
// ordinary net.Dial from inside the namespace. The host kernel owns the
// server's address and answers ARP for it, exactly as on a real NIC.
//
// Two things about veth that a physical NIC would not need: the namespace
// side has transmit checksum offload and segmentation turned off, because a
// veth hands its peer packets with checksums still to be filled in and
// segments still to be split, which the stack would reject.

func run(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestVethEcho(t *testing.T) { vethEcho(t, false) }

// TestVethTxChecksumOffload runs the echo with the checksum request forced
// on. The socket is in copy mode there, where the kernel (XDP_UMEM_TX_SW_CSUM)
// computes the checksum from our metadata exactly as a NIC would, so a wrong
// csum_start, a stale metadata word, or a bad pseudo-header partial shows up
// as the peer dropping every segment.
func TestVethTxChecksumOffload(t *testing.T) { vethEcho(t, true) }

// vethOnStart, when set by a test, sees the runtime as soon as it is up.
var vethOnStart func(*netstackRuntime)

func vethEcho(t *testing.T, offload bool) { vethEchoWithCheck(t, offload, xdplink.TxFIFO, nil) }

// vethEchoWithCheck is vethEcho with a transmit mode and an extra check on
// the runtime at the end.
func vethEchoWithCheck(t *testing.T, offload bool, tm xdplink.TxMode, check func(*testing.T, *netstackRuntime)) {
	if offload {
		old := txCsumMode
		txCsumMode = "on"
		t.Cleanup(func() { txCsumMode = old })
	}
	host, h := vethLab(t, "10.99.0.1", "10.99.0.2")

	const port = 8080
	prefix := netip.MustParsePrefix("10.99.0.1/24")
	rt, err := startNetstack(host, 0, prefix, port, 1500, tm, false, true, true, nil, afxdp.WithGenericMode())
	if err == nil && vethOnStart != nil {
		vethOnStart(rt)
	}
	if err != nil {
		t.Fatalf("startNetstack: %v", err)
	}
	var srv server
	defer func() { srv.stopping.Store(true); rt.stop() }()
	go srv.serve(rt.ln, "echo")

	c := dialInNetns(t, h, fmt.Sprintf("10.99.0.1:%d", port))
	defer c.Close()

	// Small messages, well under one segment, so nothing needs the
	// segmentation that was turned off above.
	buf := make([]byte, 1000)
	for i := 0; i < 200; i++ {
		msg := []byte(fmt.Sprintf("hello over netstack %04d ", i))
		msg = append(msg, bytes.Repeat([]byte{byte(i)}, 500)...)
		if _, err := c.Write(msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, err := io.ReadFull(c, buf[:len(msg)]); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(buf[:len(msg)], msg) {
			t.Fatalf("echo %d differs", i)
		}
	}
	// Bulk: 4 MiB through the echo, written and read concurrently, so the
	// netstack side sends packets far larger than one MSS and the endpoint has
	// to segment them (the small messages above never do).
	const bulkSize = 4 << 20
	bulkOut := make([]byte, bulkSize)
	for i := range bulkOut {
		bulkOut[i] = byte(i*13 + i>>10)
	}
	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(bulkOut)
		werr <- err
	}()
	bulkIn := make([]byte, bulkSize)
	if _, err := io.ReadFull(c, bulkIn); err != nil {
		t.Fatalf("bulk read: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	if !bytes.Equal(bulkIn, bulkOut) {
		t.Fatal("bulk echo differs")
	}
	if es := rt.ep.Stats(); es.GSOPackets == 0 {
		t.Errorf("bulk echo did not exercise GSO: %+v", es)
	} else {
		t.Logf("gso: %d packets -> %d frames", es.GSOPackets, es.GSOFrames)
	}
	c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for srv.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.accepted.Load() != 1 || srv.closed.Load() != 1 {
		t.Errorf("server saw accepted %d closed %d, want 1 and 1", srv.accepted.Load(), srv.closed.Load())
	}
	es := rt.ep.Stats()
	if es.Rx == 0 || es.Tx == 0 {
		t.Errorf("endpoint moved nothing: %+v", es)
	}
	if es.UnknownMAC != 0 || es.WrongVLAN != 0 || es.TxDropped != 0 {
		t.Errorf("endpoint drops: %+v", es)
	}
	if n := rt.s.Stats().TCP.ChecksumErrors.Value(); n != 0 {
		t.Errorf("netstack saw %d checksum errors", n)
	}
	if n := rt.s.Stats().TCP.Retransmits.Value(); n != 0 {
		t.Logf("note: %d retransmits over the veth", n)
	}
	if check != nil {
		check(t, rt)
	}
	switch {
	case offload && (es.CsumOffloaded == 0 || es.CsumSoftware != 0):
		t.Errorf("offload on, but checksums: nic %d software %d", es.CsumOffloaded, es.CsumSoftware)
	case !offload && es.CsumOffloaded != 0:
		t.Errorf("offload off, but %d frames were stamped for the NIC", es.CsumOffloaded)
	default:
		t.Logf("tx checksums: nic %d software %d", es.CsumOffloaded, es.CsumSoftware)
	}
}

// dialInNetns connects from inside the namespace. The socket is created on
// the thread that entered the namespace, which is why the goroutine is locked
// for the duration; the connection works from any thread afterwards.
func dialInNetns(t *testing.T, h netns.NsHandle, addr string) net.Conn {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("current netns: %v", err)
	}
	defer orig.Close()
	if err := netns.Set(h); err != nil {
		t.Fatalf("enter netns: %v", err)
	}
	defer netns.Set(orig)
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s from the namespace: %v", addr, err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

// vethLab builds a veth pair with the peer in its own namespace, gives both
// ends an address, and returns the host-side device name and a handle on the
// namespace to dial from. Everything is torn down with the test.
func vethLab(t *testing.T, hostIP, peerIP string) (string, netns.NsHandle) {
	t.Helper()
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
	host, peer, ns := "nsh"+suffix, "nsp"+suffix, "ns"+suffix
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(v); err != nil {
		t.Skipf("cannot create veth pair (need CAP_NET_ADMIN): %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(v) })
	run(t, "ip", "netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })

	run(t, "ip", "link", "set", peer, "netns", ns)
	run(t, "ip", "-n", ns, "addr", "add", peerIP+"/24", "dev", peer)
	run(t, "ip", "-n", ns, "link", "set", "lo", "up")
	run(t, "ip", "-n", ns, "link", "set", peer, "up")
	run(t, "ip", "netns", "exec", ns, "ethtool", "-K", peer, "tx", "off", "tso", "off", "gso", "off")
	run(t, "ip", "addr", "add", hostIP+"/24", "dev", host)
	run(t, "ip", "link", "set", host, "up")

	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatalf("netns: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return host, h
}
