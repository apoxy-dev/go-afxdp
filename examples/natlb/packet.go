//go:build linux

package main

import (
	"encoding/binary"
	"net/netip"
)

// Offsets and constants we need. Everything here is IPv4 + TCP; anything else
// is rejected by parse rather than guessed at.
const (
	etherLen  = 14
	etherType = 12

	etherIPv4 = 0x0800
	etherVLAN = 0x8100

	protoTCP = 6

	// TCP flag bits, at byte 13 of the TCP header.
	flagFIN = 0x01
	flagSYN = 0x02
	flagRST = 0x04
	flagACK = 0x10
)

// pkt is what one frame turned out to be. The offsets are into the frame, so
// the rewrite below can work in place without parsing anything twice.
type pkt struct {
	l3, l4       int // start of the IPv4 header, start of the TCP header
	end          int // end of the TCP segment, from the IPv4 total length
	src, dst     netip.Addr
	sport, dport uint16
	flags        uint8
}

// parseErr says why a frame was not something we can translate. The datapath
// counts these separately: "not for us" is ordinary, "malformed" is not.
type parseErr int

const (
	parseOK parseErr = iota
	parseTooShort
	parseNotIPv4
	parseNotTCP
	parseFragment
)

// parse pulls the IPv4/TCP tuple out of an Ethernet frame.
//
// The IPv4 header length comes from the IHL field rather than being assumed to
// be 20: a packet carrying options would otherwise have its "TCP header" read
// out of the middle of the IP header, and the rewrite would corrupt it.
func parse(f []byte) (pkt, parseErr) {
	var p pkt
	if len(f) < etherLen+20+20 {
		return p, parseTooShort
	}
	l3 := etherLen
	et := binary.BigEndian.Uint16(f[etherType:])
	if et == etherVLAN {
		// One 802.1Q tag pushes everything four bytes further in. We keep the
		// tag on the way out: the rewrite never changes the length.
		if len(f) < etherLen+4+20+20 {
			return p, parseTooShort
		}
		et = binary.BigEndian.Uint16(f[etherType+4:])
		l3 = etherLen + 4
	}
	if et != etherIPv4 || f[l3]>>4 != 4 {
		return p, parseNotIPv4
	}
	ihl := int(f[l3]&0x0f) * 4
	if ihl < 20 || l3+ihl+20 > len(f) {
		return p, parseTooShort
	}
	if f[l3+9] != protoTCP {
		return p, parseNotTCP
	}
	// Fragments carry no usable TCP header after the first, and translating
	// only the first would rewrite an address the rest still carries. Refuse
	// both halves rather than corrupt a flow: MF set, or a non-zero offset.
	frag := binary.BigEndian.Uint16(f[l3+6:])
	if frag&0x2000 != 0 || frag&0x1fff != 0 {
		return p, parseFragment
	}
	// The total length drives where the TCP segment ends, and it comes off the
	// wire like everything else here. Left unchecked it lets a frame through
	// that rewrite will translate and tcpChecksum will then decline to
	// checksum, putting a corrupt packet on the wire and spending a NAT port
	// on it. Bound it here, with every other malformed shape.
	total := int(binary.BigEndian.Uint16(f[l3+2:]))
	if total < ihl+20 || l3+total > len(f) {
		return p, parseTooShort
	}
	l4 := l3 + ihl
	p = pkt{
		l3:    l3,
		l4:    l4,
		end:   l3 + total,
		src:   netip.AddrFrom4([4]byte(f[l3+12 : l3+16])),
		dst:   netip.AddrFrom4([4]byte(f[l3+16 : l3+20])),
		sport: binary.BigEndian.Uint16(f[l4:]),
		dport: binary.BigEndian.Uint16(f[l4+2:]),
		flags: f[l4+13],
	}
	return p, parseOK
}

// rewrite replaces the Ethernet addresses, the IPv4 addresses and the TCP
// ports in place, then fixes both checksums. The frame keeps its length, which
// is what lets the datapath transmit the very frame it received.
func rewrite(f []byte, p pkt, smac, dmac [6]byte, src, dst netip.Addr, sport, dport uint16) {
	copy(f[0:6], dmac[:])
	copy(f[6:12], smac[:])

	s4, d4 := src.As4(), dst.As4()
	copy(f[p.l3+12:p.l3+16], s4[:])
	copy(f[p.l3+16:p.l3+20], d4[:])
	binary.BigEndian.PutUint16(f[p.l4:], sport)
	binary.BigEndian.PutUint16(f[p.l4+2:], dport)

	ipChecksum(f, p.l3)
	tcpChecksum(f, p.l3, p.l4, p.end)
}

// ipChecksum recomputes the IPv4 header checksum from scratch.
//
// Incremental update is the usual trick and this is a translation, so every
// changed field is known -- but a header is twenty-odd bytes and recomputing it
// is a handful of adds. Getting it obviously right matters more here than
// saving them, and it cannot drift out of step with the rewrite above.
func ipChecksum(f []byte, l3 int) {
	ihl := int(f[l3]&0x0f) * 4
	f[l3+10], f[l3+11] = 0, 0
	binary.BigEndian.PutUint16(f[l3+10:], onesComplement(sum(f[l3:l3+ihl], 0)))
}

// tcpChecksum recomputes the TCP checksum over the pseudo-header and the whole
// segment. end comes from parse, which has already checked it against both the
// header length and the frame, so there is no untrusted arithmetic here and no
// path that returns without writing a checksum -- a frame whose headers were
// rewritten must never go out carrying the old one.
//
// end matters because a frame with trailing padding (an Ethernet minimum-size
// frame, say) must not have the padding summed, which is the classic way this
// goes subtly wrong on small packets.
func tcpChecksum(f []byte, l3, l4, end int) {
	seg := f[l4:end]
	seg[16], seg[17] = 0, 0

	// Pseudo-header: source and destination address, a zero, the protocol,
	// and the TCP length.
	var s uint32
	s = sum(f[l3+12:l3+20], s)
	s += uint32(protoTCP)
	s += uint32(len(seg))
	binary.BigEndian.PutUint16(seg[16:], onesComplement(sum(seg, s)))
}

// sum adds a byte range into a running one's-complement sum, 16 bits at a
// time. An odd trailing byte is the high half of its word.
func sum(b []byte, s uint32) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	return s
}

// onesComplement folds the carries back in and inverts, which is the last step
// of every checksum here.
func onesComplement(s uint32) uint16 {
	for s>>16 != 0 {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}
