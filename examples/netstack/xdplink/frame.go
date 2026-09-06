//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package xdplink is a gVisor netstack link endpoint over go-afxdp.
//
// It implements stack.LinkEndpoint from gvisor.dev/gvisor/pkg/tcpip/stack on
// top of an *afxdp.Fleet: one receive goroutine per NIC queue moves frames
// from the AF_XDP rx ring into the stack, and the stack's transmit calls build
// frames straight into the UMEM and put them on the tx ring. The stack does
// everything above Ethernet (IPv4, TCP); this package does Ethernet, 802.1Q,
// and the bookkeeping between the two.
//
// # Where the bytes go
//
// gVisor's buffer package owns every byte the stack touches: a buffer.View
// wraps a pooled chunk it allocated itself, and there is no way to hand it a
// caller-owned slice. So a received frame is copied once, out of the UMEM
// into a pooled view, and the frame goes straight back to the fill ring. On
// transmit the packet's views are copied once, straight into a UMEM frame,
// with nothing in between. One memcpy in each direction is the floor the
// gVisor API sets; everything else AF_XDP buys is kept: no per-packet
// syscalls, batched rings, no sk_buff, no kernel TCP.
//
// # Layer 2 without ARP
//
// The endpoint answers no ARP and asks none. The XDP filter hands the stack
// only its own TCP port, so the kernel keeps seeing ARP and keeps answering it
// for the address (which the kernel owns too, on the same interface or a VLAN
// sub-interface). Outbound frames need a destination MAC anyway, so the
// endpoint remembers the source MAC of every peer it hears from, keyed by IPv4
// source address, and looks the destination up there. A server only ever
// replies to peers it has heard from, so the table always has the answer; for
// a client behind a router it correctly holds the router's MAC.
package xdplink

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	etherHdrLen   = 14
	vlanHdrLen    = 18
	etherTypeIPv4 = 0x0800
	etherTypeVLAN = 0x8100
	ipv4MinHdrLen = 20
	minFrameLen   = 60 // Ethernet minimum without FCS
)

var broadcastMAC = [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// parseFrame classifies a received Ethernet frame. It returns the length of
// the link header and the EtherType of what follows it. ok is false for a
// frame too short to hold a header, and for one whose 802.1Q tagging does not
// match the endpoint: a tagged frame on an untagged (vlan == 0) endpoint, an
// untagged frame on a tagged one, or a tag for a different VLAN. Only a single
// 802.1Q tag is understood; QinQ is treated as "some other VLAN".
func parseFrame(f []byte, vlan uint16) (hdrLen int, etherType uint16, ok bool) {
	if len(f) < etherHdrLen {
		return 0, 0, false
	}
	et := uint16(f[12])<<8 | uint16(f[13])
	if et != etherTypeVLAN {
		if vlan != 0 {
			return 0, 0, false
		}
		return etherHdrLen, et, true
	}
	if len(f) < vlanHdrLen {
		return 0, 0, false
	}
	if vid := (uint16(f[14])<<8 | uint16(f[15])) & 0x0fff; vid != vlan {
		return 0, 0, false
	}
	return vlanHdrLen, uint16(f[16])<<8 | uint16(f[17]), true
}

// linkHeaderLen is the size of the header putHeader writes for this VLAN
// setting: 14 bytes untagged, 18 with an 802.1Q tag.
func linkHeaderLen(vlan uint16) int {
	if vlan != 0 {
		return vlanHdrLen
	}
	return etherHdrLen
}

// putHeader writes an Ethernet header into b, with an 802.1Q tag when vlan is
// nonzero (priority and DEI zero), and returns its length. b must have room
// for linkHeaderLen(vlan) bytes.
func putHeader(b []byte, dst, src [6]byte, vlan uint16, etherType uint16) int {
	copy(b[0:6], dst[:])
	copy(b[6:12], src[:])
	if vlan == 0 {
		b[12], b[13] = byte(etherType>>8), byte(etherType)
		return etherHdrLen
	}
	b[12], b[13] = 0x81, 0x00
	b[14], b[15] = byte(vlan>>8)&0x0f, byte(vlan)
	b[16], b[17] = byte(etherType>>8), byte(etherType)
	return vlanHdrLen
}

// isMulticast reports whether a MAC has the group bit set (broadcast included).
func isMulticast(mac []byte) bool { return mac[0]&1 != 0 }

// neighbours is the learned IPv4-to-MAC table. Reads happen on every
// received frame (learn) and every transmitted one (AddHeader), from every
// queue; writes only when a peer is first heard from or changes MAC. So the
// table is copy-on-write: readers load a pointer to an immutable map, no lock
// and no shared cache line written on the packet path; a writer copies the
// map under its mutex and publishes the new one.
type neighbours struct {
	lastEvict int64 // when the full table last took a new entry, for evictInterval
	m         atomic.Pointer[map[[4]byte][6]byte]
	mu        sync.Mutex // writers only
}

func newNeighbours() *neighbours {
	n := &neighbours{}
	m := make(map[[4]byte][6]byte)
	n.m.Store(&m)
	return n
}

// maxNeighbours bounds the table. A server on one subnet has a handful of
// peers (or one router); on a routed network the table holds one entry per
// client address, and a burst of spoofed sources must not grow it without
// limit. Past the bound an entry is evicted for each new one, and — since
// every new entry copies the map — a full table takes at most one new
// entry per evictInterval, so a flood of forged sources costs a bounded
// amount of copying rather than one copy per frame. An evicted peer is
// relearned from its next frame.
const (
	maxNeighbours = 4096
	evictInterval = 10 * time.Millisecond
)

// learn records mac as the way to reach ip. On the packet path it is one
// pointer load and a map lookup; the copy happens only for a new or changed
// entry.
func (n *neighbours) learn(ip [4]byte, mac [6]byte) {
	if have, ok := (*n.m.Load())[ip]; ok && have == mac {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	old := *n.m.Load()
	if have, ok := old[ip]; ok && have == mac {
		return
	}
	_, known := old[ip]
	evict := !known && len(old) >= maxNeighbours
	if evict {
		now := time.Now().UnixNano()
		if now-n.lastEvict < int64(evictInterval) {
			return // full, and one entry was replaced just now: not this one
		}
		n.lastEvict = now
	}
	m := make(map[[4]byte][6]byte, len(old)+1)
	for k, v := range old {
		if evict {
			evict = false // drop whichever entry the map hands out first
			continue
		}
		m[k] = v
	}
	m[ip] = mac
	n.m.Store(&m)
}

func (n *neighbours) lookup(ip [4]byte) ([6]byte, bool) {
	mac, ok := (*n.m.Load())[ip]
	return mac, ok
}

func (n *neighbours) len() int { return len(*n.m.Load()) }
