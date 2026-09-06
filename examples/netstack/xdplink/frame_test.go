//go:build linux

package xdplink

import "testing"

func TestParseFrame(t *testing.T) {
	untagged := make([]byte, 60)
	untagged[12], untagged[13] = 0x08, 0x00
	tagged := make([]byte, 64)
	tagged[12], tagged[13] = 0x81, 0x00
	tagged[14], tagged[15] = 0x08, 0x53 // PCP 0, VID 2131
	tagged[16], tagged[17] = 0x08, 0x00
	prio := append([]byte(nil), tagged...)
	prio[14] |= 0xa0 // PCP 5: must still match the VID

	cases := []struct {
		name   string
		f      []byte
		vlan   uint16
		hdr    int
		et     uint16
		wantOK bool
	}{
		{"untagged on untagged", untagged, 0, 14, 0x0800, true},
		{"untagged on tagged", untagged, 2131, 0, 0, false},
		{"tagged on tagged", tagged, 2131, 18, 0x0800, true},
		{"tagged with priority", prio, 2131, 18, 0x0800, true},
		{"tagged other vlan", tagged, 2154, 0, 0, false},
		{"tagged on untagged", tagged, 0, 0, 0, false},
		{"runt", untagged[:13], 0, 0, 0, false},
		{"runt tagged", tagged[:17], 2131, 0, 0, false},
	}
	for _, c := range cases {
		hdr, et, ok := parseFrame(c.f, c.vlan)
		if ok != c.wantOK || hdr != c.hdr || et != c.et {
			t.Errorf("%s: parseFrame = (%d, %#x, %v), want (%d, %#x, %v)",
				c.name, hdr, et, ok, c.hdr, c.et, c.wantOK)
		}
	}
}

func TestPutHeaderRoundTrip(t *testing.T) {
	dst := [6]byte{1, 2, 3, 4, 5, 6}
	src := [6]byte{0xa, 0xb, 0xc, 0xd, 0xe, 0xf}
	for _, vlan := range []uint16{0, 2131, 4095} {
		b := make([]byte, 64)
		n := putHeader(b, dst, src, vlan, etherTypeIPv4)
		if n != linkHeaderLen(vlan) {
			t.Fatalf("vlan %d: wrote %d bytes, want %d", vlan, n, linkHeaderLen(vlan))
		}
		hdr, et, ok := parseFrame(b, vlan)
		if !ok || hdr != n || et != etherTypeIPv4 {
			t.Fatalf("vlan %d: parse of own header = (%d, %#x, %v)", vlan, hdr, et, ok)
		}
		if [6]byte(b[0:6]) != dst || [6]byte(b[6:12]) != src {
			t.Fatalf("vlan %d: MACs not written", vlan)
		}
	}
}

func TestNeighbours(t *testing.T) {
	n := newNeighbours()
	ip := [4]byte{10, 0, 0, 1}
	if _, ok := n.lookup(ip); ok {
		t.Fatal("empty table answered")
	}
	m1 := [6]byte{1, 1, 1, 1, 1, 1}
	n.learn(ip, m1)
	n.learn(ip, m1) // second time takes the read-only path
	if got, ok := n.lookup(ip); !ok || got != m1 {
		t.Fatalf("lookup = %v, %v", got, ok)
	}
	m2 := [6]byte{2, 2, 2, 2, 2, 2}
	n.learn(ip, m2) // a peer that moved
	if got, _ := n.lookup(ip); got != m2 {
		t.Fatalf("update not applied: %v", got)
	}
	if n.len() != 1 {
		t.Fatalf("len = %d", n.len())
	}
}
