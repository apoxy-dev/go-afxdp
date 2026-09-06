//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package xdplink

import (
	"sync/atomic"
	"unsafe"

	"github.com/atoonk/go-afxdp"
)

// Transmit without copying payload.
//
// A TxCache is content that already lives in the fleet's shared UMEM
// (afxdp.WithSharedMemory), laid out so the NIC can send it as it is: one
// UMEM frame per unit of payload, with room in front of the payload for the
// headers of whichever connection sends it next.
//
//	frame:  [ metadata 16 ][ header room ][ payload ≤ MSS ... ]
//	         ^base           ^                ^base+PayloadOffset
//	                         Addr = base+PayloadOffset-hdrLen
//
// The application hands a frame's payload to TCP as an external view
// (buffer.NewViewFromExternal, tcp.Endpoint.WriteExternal in the fork), so
// the packet that reaches WritePackets carries a view that points straight
// into the cache. The endpoint recognises it, writes the packet's headers
// into the room before it, and transmits the frame itself: no copy, one
// descriptor. The header room is per frame, so a frame can be on the wire
// for one sender at a time: Acquire marks it in flight, and the fleet's
// foreign completion (afxdp.WithOnForeignComplete → Release) frees it.
// A frame that is busy — or a payload that is not a whole frame's — takes the
// copying path as before, so correctness never depends on the cache.
type TxCache struct {
	base    uint64 // UMEM address of the first frame
	mem     []byte // the frames, base-relative
	fs      int    // frame size
	mask    int    // fs-1 when fs is a power of two, else -1
	off     int    // PayloadOffset
	n       int
	plen    int // payload bytes per frame the application streams (SetPayloadLen)
	busy    []atomic.Bool
	lo, hi  uintptr // address range of mem, for Locate
	ZeroCpy atomic.Uint64
}

// TxCachePayloadOffset is where a frame's payload starts: after the
// transmit metadata and the largest headers a segment can carry (an 802.1Q
// link header, IPv4 with options, TCP with options), rounded up.
const TxCachePayloadOffset = 160

// NewTxCache lays frames out over the fleet's extra area: as many as fit.
func NewTxCache(fleet *afxdp.Fleet) *TxCache {
	base, mem := fleet.Extra()
	if mem == nil {
		return nil
	}
	fs := fleet.Sockets()[0].FrameSize()
	n := len(mem) / fs
	c := &TxCache{base: base, mem: mem[:n*fs], fs: fs, mask: -1, off: TxCachePayloadOffset, n: n, busy: make([]atomic.Bool, n)}
	if fs&(fs-1) == 0 {
		c.mask = fs - 1
	}
	c.lo = uintptr(unsafe.Pointer(&mem[0]))
	c.hi = c.lo + uintptr(n*fs)
	return c
}

// Frames is how many payload frames the cache holds.
func (c *TxCache) Frames() int { return c.n }

// PayloadCap is the most payload one frame holds.
func (c *TxCache) PayloadCap() int { return c.fs - c.off }

// SetPayloadLen records how many payload bytes each frame carries on the
// wire — one segment's worth, the same for every frame — which is what makes
// a Span's logical bytes map back to frames.
func (c *TxCache) SetPayloadLen(n int) { c.plen = n }

// PayloadLen is the per-frame payload SetPayloadLen recorded.
func (c *TxCache) PayloadLen() int { return c.plen }

// Span returns the memory from frame i's payload offset to the end of frame
// i+count-1: what a strided external view (buffer.NewViewFromExternalSized)
// is made over, whose logical size is count*PayloadLen. Logical byte L of
// such a view is byte L%PayloadLen of frame i+L/PayloadLen's payload.
func (c *TxCache) Span(i, count int) []byte {
	return c.mem[i*c.fs+c.off : (i+count)*c.fs]
}

// piece is a run of a strided view's logical window that lies in one frame.
type piece struct {
	frame   int // cache frame index
	off     int // payload offset within the frame, 0 = at the frame's payload start
	n       int // bytes
	inPlace bool
}

// pieces splits the window [read, write) of a strided view over span into
// per-frame pieces, appending to dst. A piece that starts at a frame's
// payload start can go out in place; one that starts mid-frame (the stack
// split a segment there) cannot, and is copied. ok is false when span is not
// one of this cache's spans or PayloadLen is unset.
func (c *TxCache) pieces(dst []piece, span []byte, read, write int) ([]piece, bool) {
	if c.plen == 0 || len(span) == 0 {
		return dst, false
	}
	a := uintptr(unsafe.Pointer(&span[0]))
	if a < c.lo || a >= c.hi {
		return dst, false
	}
	rel := int(a - c.lo)
	var in int
	if c.mask >= 0 {
		in = rel & c.mask
	} else {
		in = rel % c.fs
	}
	if in != c.off {
		return dst, false
	}
	f0 := (rel - in) / c.fs
	for l := read; l < write; {
		fr, o := f0+l/c.plen, l%c.plen
		n := c.plen - o
		if l+n > write {
			n = write - l
		}
		dst = append(dst, piece{frame: fr, off: o, n: n, inPlace: o == 0})
		l += n
	}
	return dst, true
}

// Payload returns frame i's payload area (PayloadCap bytes); the caller
// fills it once and then hands slices of it to TCP.
func (c *TxCache) Payload(i int) []byte {
	return c.mem[i*c.fs+c.off : (i+1)*c.fs]
}

// Locate reports whether p is the payload of one frame, starting at its
// payload offset — the shape WritePackets can transmit in place — and which.
func (c *TxCache) Locate(p []byte) (idx int, ok bool) {
	if len(p) == 0 {
		return 0, false
	}
	a := uintptr(unsafe.Pointer(&p[0]))
	if a < c.lo || a >= c.hi {
		return 0, false
	}
	rel := int(a - c.lo)
	var in int
	if c.mask >= 0 {
		in = rel & c.mask
	} else {
		in = rel % c.fs
	}
	if in != c.off || len(p) > c.fs-c.off {
		return 0, false
	}
	return (rel - in) / c.fs, true
}

// Acquire marks frame i in flight; false means it already is.
func (c *TxCache) Acquire(i int) bool { return c.busy[i].CompareAndSwap(false, true) }

// Release is the fleet's OnForeignComplete: the NIC is done with the frame
// at addr.
func (c *TxCache) Release(addr uint64) {
	if addr < c.base {
		return
	}
	if i := int((addr - c.base) / uint64(c.fs)); i < c.n {
		c.busy[i].Store(false)
	}
}

// frame returns the whole UMEM frame i, base-relative, and its address.
func (c *TxCache) frame(i int) (addr uint64, f []byte) {
	return c.base + uint64(i*c.fs), c.mem[i*c.fs : (i+1)*c.fs]
}
