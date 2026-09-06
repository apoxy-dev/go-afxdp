//go:build linux && gvisorfork

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"testing"
	"time"

	afxdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
)

// TestVethPorts serves four consecutive ports from one stack and echoes on
// each: the XDP filter must pass them all and every port needs its own
// listener (see -ports).
func TestVethPorts(t *testing.T) {
	const base = 9300
	old := portCount
	portCount = 4
	t.Cleanup(func() { portCount = old })

	host, h := vethLab(t, "10.96.0.1", "10.96.0.2")
	srv := &server{}
	rt, err := startNetstack(host, 0, netip.MustParsePrefix("10.96.0.1/24"), base, 1500,
		xdplink.TxDirect, true, true, true,
		func() *rtcServer { return newRTCServer(srv) }, afxdp.WithGenericMode())
	if err != nil {
		t.Fatalf("startNetstack: %v", err)
	}
	defer func() { srv.stopping.Store(true); rt.stop() }()

	msg := []byte("ports")
	for i := 0; i < portCount; i++ {
		addr := fmt.Sprintf("10.96.0.1:%d", base+i)
		c := dialInNetns(t, h, addr)
		if _, err := c.Write(msg); err != nil {
			t.Fatalf("%s: write: %v", addr, err)
		}
		buf := make([]byte, len(msg))
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("%s: read: %v", addr, err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatalf("%s echoed %q, want %q", addr, buf, msg)
		}
		c.Close()
	}
	if n := srv.accepted.Load(); n != uint64(portCount) {
		t.Errorf("server accepted %d connections, want one per port (%d)", n, portCount)
	}
}
