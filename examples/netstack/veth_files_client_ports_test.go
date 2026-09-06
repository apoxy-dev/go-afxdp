//go:build linux && gvisorfork

package main

import (
	"net/netip"
	"os"
	"testing"
	"time"

	afxdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// TestVethFilesClientPorts is the churn client with source ports chosen from
// a table, as on a real NIC, over a range small enough that every
// connection wraps its port cycle several times in a run. A port comes
// back around long after its previous connection closed; TIME-WAIT reuse
// and a short client TIME-WAIT must make that fine. When it is not, every
// connection dies with "port is in use" the moment the cycle wraps.
func TestVethFilesClientPorts(t *testing.T) {
	host, h := vethLab(t, "10.94.0.1", "10.94.0.2")
	const mix = "3k:1,64k:1"
	fs, err := newFileSet(mix)
	if err != nil {
		t.Fatal(err)
	}
	ln := listenInNetns(t, h, "10.94.0.2:8080")
	srv := &server{files: fs}
	go srv.serve(ln, "files")
	t.Cleanup(func() { srv.stopping.Store(true); ln.Close() })

	peerMAC, err := netlinkMAC(t, h, "10.94.0.2")
	if err != nil {
		t.Skipf("cannot read the peer's MAC: %v", err)
	}
	oldMAC, oldVerify, oldOpts := peerMACFlag, filesVerify, clientXDPOptions
	oldRSS, oldLo, oldHi := clientTestRSS, clientPortLo, clientPortHi
	peerMACFlag, filesVerify = peerMAC, true
	clientXDPOptions = []afxdp.Option{afxdp.WithGenericMode()}
	// One queue, every port hashes to it, and a cycle of 1200 ports that a
	// veth's 6000 dials a second wraps inside the 200 ms TIME-WAIT: every
	// wrap finds its ports still held, and the client must wait them out,
	// not die. (A ramped start on an idle 100G server does the same.)
	clientTestRSS = &rss{key: make([]byte, 40), indir: []int{0}}
	clientPortLo, clientPortHi = 20000, 21200
	t.Cleanup(func() {
		peerMACFlag, filesVerify, clientXDPOptions = oldMAC, oldVerify, oldOpts
		clientTestRSS, clientPortLo, clientPortHi = oldRSS, oldLo, oldHi
	})

	if os.Getenv("BP_DANGLING") != "" {
		old := tcpip.TrackDanglingEndpoints
		tcpip.TrackDanglingEndpoints = true
		t.Cleanup(func() { tcpip.TrackDanglingEndpoints = old })
	}
	done := make(chan *filesStats, 1)
	go func() {
		done <- runNetstackFilesClient(filesConfig{
			target: "10.94.0.2:8080", workers: 8, perConn: 1,
			duration: 5 * time.Second, mix: mix, ramp: 100 * time.Millisecond,
		}, host, 0, netip.MustParsePrefix("10.94.0.1/24"), 1500, xdplink.TxDirect, true, true)
	}()
	var st *filesStats
	select {
	case st = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the client did not finish")
	}
	if n := st.errs.Load(); n != 0 {
		t.Errorf("client reported %d errors (the log above says what)", n)
	}
	if c := st.conns.Load(); c < 3000 {
		t.Errorf("client made %d connections in 5 s; the cycle should wrap many times, paced by TIME-WAIT, not stop", c)
	}
	t.Logf("%d connections, %d objects, %d errors", st.conns.Load(), st.reqs.Load(), st.errs.Load())
}
