//go:build linux && gvisorfork

package main

import (
	"bufio"
	"bytes"
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

// TestVethFilesRTC fetches every object of a mix, several times over one
// connection and over fresh ones, from the zero-copy file server, and checks
// each byte against the pattern.
func TestVethFilesRTC(t *testing.T) {
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
	host, peer, ns := "fih"+suffix, "fip"+suffix, "fi"+suffix
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(v); err != nil {
		t.Skipf("cannot create veth pair (need CAP_NET_ADMIN): %v", err)
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

	const mix = "3k:1,64k:1,700k:1"
	fs, err := newFileSet(mix)
	if err != nil {
		t.Fatal(err)
	}
	old1, old2, old3 := cacheBytes, rtcFiles, cacheMTU
	cacheBytes, rtcFiles = fs.cacheFramesNeeded(2048, 1420, 2)+1<<20, true
	t.Cleanup(func() { cacheBytes, rtcFiles, cacheMTU = old1, old2, old3 })

	const port = 8080
	prefix := netip.MustParsePrefix("10.97.0.1/24")
	var srv server
	srv.files = fs
	rt, err := startNetstack(host, 0, prefix, port, 1500, xdplink.TxDirect, true, true, true,
		func() *rtcServer { r := newRTCServer(&srv); r.files = fs; return r }, afxdp.WithGenericMode())
	if err != nil {
		t.Fatalf("startNetstack: %v", err)
	}
	defer func() { srv.stopping.Store(true); rt.stop() }()
	if err := fs.layoutInCache(rt.cache, cachePayload(rt.cache), 2); err != nil {
		t.Fatal(err)
	}

	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatalf("netns: %v", err)
	}
	defer h.Close()
	fetchAll := func(c io.ReadWriter) {
		for _, cl := range fs.classes {
			if _, err := io.WriteString(c, "GET /"+cl.name+"\n"); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := make([]byte, cl.bytes)
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatalf("read %s: %v", cl.name, err)
			}
			if !bytes.Equal(got, fs.body[:cl.bytes]) {
				t.Fatalf("object %s differs from the pattern", cl.name)
			}
		}
	}
	for conn := 0; conn < 3; conn++ {
		c := dialInNetns(t, h, fmt.Sprintf("10.97.0.1:%d", port))
		c.SetDeadline(time.Now().Add(20 * time.Second))
		rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
		for rep := 0; rep < 3; rep++ {
			fetchAll(struct {
				io.Reader
				io.Writer
			}{rw, flushWriter{rw}})
		}
		c.Close()
	}
	es := rt.ep.Stats()
	if es.TxZeroCopy == 0 {
		t.Fatalf("nothing served in place: %+v", es)
	}
	t.Logf("frames in place %d, copied %d", es.TxZeroCopy, es.Tx-es.TxZeroCopy)
}

type flushWriter struct{ w *bufio.ReadWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.w.Flush()
	}
	return n, err
}
