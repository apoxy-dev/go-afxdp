//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package afxdp

import (
	"fmt"
	"syscall"
)

// Shared UMEM: one memory region registered with every socket of a Fleet.
//
// By default each socket maps its own UMEM, so a frame address means
// something only to the socket it belongs to. WithSharedMemory maps one
// region for the whole fleet instead — every queue's frames laid out one
// after another, then an extra area the fleet never touches — and registers
// that same region with each socket (the way VPP registers its buffer pool
// with every AF_XDP socket it opens). Each socket still owns its own frames
// (its fill ring and transmit pool draw only from its slice), but an address
// is the same on every socket, and a frame in the extra area can be
// transmitted by any of them without a copy: build the packet there, hand
// Transmit a descriptor pointing at it, and take it back when the address
// comes out of the completion ring, which is reported to
// WithOnForeignComplete instead of going into a pool.
//
// That is what a userspace TCP stack serving content out of memory wants: a
// cache of ready-to-send frames that any queue can put on the wire.

// WithSharedMemory gives the fleet one UMEM for all its sockets, with extra
// bytes (rounded up to whole frames) after the sockets' frames for the
// caller's use; Extra returns that area. Zero extra is allowed: the sockets
// then simply share one mapping.
func WithSharedMemory(extra int) Option {
	return func(c *config) { c.sharedMemory = true; c.sharedExtra = extra }
}

// The shared region is one mapping, faulted in by whichever thread runs Open:
// on a two-node box it lands on that thread's node, not each queue's, unlike
// the per-queue UMEMs (see Fleet's NUMA placement). Content served from the
// extra area is read by the NIC's DMA, so place the process, or accept the
// cross-node read, when the cache is large.

// WithOnForeignComplete names the function told, from within Complete, of
// each transmitted frame that is not from a socket's own pool — a frame in
// the shared region's extra area. It runs on the goroutine that called
// Complete (a transmit path), so it must be quick and must not call back
// into the socket; an atomic decrement is the intended shape.
func WithOnForeignComplete(f func(addr uint64)) Option {
	return func(c *config) { c.opts.OnForeignComplete = f }
}

// Memory returns the fleet's shared UMEM, or nil without WithSharedMemory.
// Addresses (in Desc, and reported to OnForeignComplete) are offsets into it.
func (f *Fleet) Memory() []byte { return f.shared }

// Extra returns the caller's area of the shared UMEM and its address (the
// offset of its first byte), or nil and 0 without WithSharedMemory. Frames
// built there must keep to the same rules as any other: a descriptor stays
// within one FrameSize-aligned chunk, and with transmit metadata on, leaves
// the metadata gap before its address.
func (f *Fleet) Extra() (addr uint64, mem []byte) {
	if f.shared == nil || f.extraAddr >= uint64(len(f.shared)) {
		return 0, nil
	}
	return f.extraAddr, f.shared[f.extraAddr:]
}

// mapSharedMemory maps the region for nQueues sockets plus extra bytes.
func mapSharedMemory(nQueues int, opts *Options, extra int) ([]byte, uint64, error) {
	if extra < 0 {
		return nil, 0, fmt.Errorf("afxdp: WithSharedMemory(%d): the extra area cannot be negative", extra)
	}
	fs := opts.FrameSize
	extra = (extra + fs - 1) / fs * fs
	perSocket := opts.NumFrames * fs
	total := nQueues*perSocket + extra
	mem, err := syscall.Mmap(-1, 0, total, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_POPULATE)
	if err != nil {
		return nil, 0, err
	}
	return mem, uint64(nQueues * perSocket), nil
}
