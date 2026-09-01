//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/atoonk/go-afxdp"
)

// The rig link is tagged, so the redirect filter has to see past an 802.1Q tag
// or nothing reaches natlb at all.
func TestFilterMatchesTaggedTCP(t *testing.T) {
	// MatchPacket compiles the filter and loads it as a BPF program, which
	// needs privileges. Without them this is not a failure, just a test that
	// cannot run here.
	if os.Geteuid() != 0 {
		t.Skip("loading a BPF program needs root")
	}
	plain := tcpFrame(macCli, macLB, client, vipIP, 5678, 443, flagSYN, nil)

	// Same packet with one VLAN tag spliced in after the MACs.
	tagged := make([]byte, 0, len(plain)+4)
	tagged = append(tagged, plain[:12]...)
	tagged = append(tagged, 0x81, 0x00, 0x08, 0x05) // TPID 0x8100, VLAN 2053
	tagged = append(tagged, plain[12:]...)

	opts := []afxdp.Option{afxdp.WithFilter(
		afxdp.MatchTCPPort(80, 443),
		afxdp.MatchTCPSrcPort(80, 443),
	)}
	for _, tc := range []struct {
		name string
		pkt  []byte
	}{{"untagged", plain}, {"vlan 2053", tagged}} {
		ok, err := afxdp.MatchPacket(tc.pkt, opts...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !ok {
			t.Errorf("%s: the filter did not redirect a TCP/443 packet", tc.name)
		}
	}

	// And our own parser must read the tagged one correctly.
	p, perr := parse(tagged)
	if perr != parseOK {
		t.Fatalf("parse(tagged): %v", perr)
	}
	if p.l3 != 18 || p.dport != 443 || p.dst != vipIP {
		t.Errorf("tagged parse: l3=%d dport=%d dst=%s", p.l3, p.dport, p.dst)
	}
	if binary.BigEndian.Uint16(tagged[12:]) != 0x8100 {
		t.Error("tag was lost")
	}
}
