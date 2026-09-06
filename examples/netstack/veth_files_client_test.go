//go:build linux && gvisorfork

package main

import (
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	afxdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"github.com/vishvananda/netns"
)

// TestVethFilesClient drives the file workload from the netstack client
// against an ordinary kernel server in the peer's namespace: every object
// is checked byte for byte, connections are closed and dialled again, and
// nothing may be counted as an error. On a veth the RSS table is
// unreadable, so this also exercises the single-stack fallback (one stack
// over all queues, ports chosen by the stack).
func TestVethFilesClient(t *testing.T) {
	host, h := vethLab(t, "10.95.0.1", "10.95.0.2")

	const mix = "3k:1,64k:1,300k:1"
	fs, err := newFileSet(mix)
	if err != nil {
		t.Fatal(err)
	}
	// The server: an ordinary listener inside the namespace, serving the
	// same GET /<name> protocol.
	ln := listenInNetns(t, h, "10.95.0.2:8080")
	srv := &server{files: fs}
	go srv.serve(ln, "files")
	t.Cleanup(func() { srv.stopping.Store(true); ln.Close() })

	peerMAC, err := netlinkMAC(t, h, "10.95.0.2")
	if err != nil {
		t.Skipf("cannot read the peer's MAC: %v", err)
	}
	oldMAC, oldVerify := peerMACFlag, filesVerify
	peerMACFlag, filesVerify = peerMAC, true
	t.Cleanup(func() { peerMACFlag, filesVerify = oldMAC, oldVerify })

	oldOpts := clientXDPOptions
	clientXDPOptions = []afxdp.Option{afxdp.WithGenericMode()}
	t.Cleanup(func() { clientXDPOptions = oldOpts })

	done := make(chan *filesStats, 1)
	go func() {
		done <- runNetstackFilesClient(filesConfig{
			target: "10.95.0.2:8080", workers: 8, perConn: 3,
			duration: 4 * time.Second, mix: mix,
		}, host, 0, netip.MustParsePrefix("10.95.0.1/24"), 1500, xdplink.TxDirect, true, true)
	}()
	var st *filesStats
	select {
	case st = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the client did not finish")
	}
	if st == nil {
		t.Skip("client could not start on this veth")
	}
	if n := st.errs.Load(); n != 0 {
		t.Errorf("client reported %d errors (the log above says what)", n)
	}
	if n := st.reqs.Load(); n < 20 {
		t.Fatalf("client fetched %d objects, want a working stream", n)
	}
	if n := st.conns.Load(); n < 8 {
		t.Errorf("client made %d connections, want at least the %d it started with", n, 8)
	}
	// Bytes must account for every finished object exactly, plus at most
	// one unfinished one per connection (the run ends mid-object).
	for i := range st.perClass {
		cs := &st.perClass[i]
		done := cs.count.Load() * uint64(fs.classes[i].bytes)
		got := cs.bytes.Load()
		if got < done || got-done >= uint64(8*fs.classes[i].bytes) {
			t.Errorf("%s: %d bytes for %d finished objects of %d bytes", cs.name, got, cs.count.Load(), fs.classes[i].bytes)
		}
	}
	t.Logf("%d objects over %d connections, %d bytes, verified", st.reqs.Load(), st.conns.Load(), st.bytes.Load())
}

// listenInNetns opens a listener inside a namespace; the socket belongs to
// that namespace afterwards, so it can be served from any thread.
func listenInNetns(t *testing.T, h netns.NsHandle, addr string) net.Listener {
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
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen in the namespace: %v", err)
	}
	return ln
}

// netlinkMAC provokes the kernel's ARP for ip and returns the MAC it learns,
// the way the client does when it has no -peer-mac.
func netlinkMAC(t *testing.T, h netns.NsHandle, ip string) (string, error) {
	t.Helper()
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", err
	}
	mac, err := kernelNeighbour(addr)
	if err != nil {
		return "", err
	}
	return net.HardwareAddr(mac[:]).String(), nil
}
