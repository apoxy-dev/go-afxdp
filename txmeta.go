//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package afxdp

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Transmit metadata (Linux 6.8+): a small header the kernel reads from the
// bytes immediately before a transmit frame, through which the application
// asks the NIC to finish a transport checksum (or timestamp the frame). With
// WithTxMetadata every transmit frame is laid out as
//
//	[ metadata: TxMetadataLen bytes ][ packet: FrameSize - TxMetadataLen bytes ]
//
// inside its UMEM chunk. Alloc and SendFunc hand out the packet part; TxMeta
// finds the metadata part for a frame; the descriptor carries XDP_TX_METADATA.
// The kernel acts only on the metadata's Flags, so a frame that asks for
// nothing is sent exactly as before.
//
// Whether the driver honours a checksum request is not visible at send time
// — one that does not simply transmits the frame with the checksum field as
// written. Ask first with QueryXSKFeatures.

// txMetadataLen is the size of struct xsk_tx_metadata.
const txMetadataLen = 16

// xdpUmemTxSwCsum is XDP_UMEM_TX_SW_CSUM (Linux 6.8), which the x/sys this
// module pins does not define: on a copy-mode socket the kernel computes a
// requested checksum in software, so tests on veth behave like hardware.
const xdpUmemTxSwCsum = 1 << 1

// TxMetadata is what TxMeta holds, in the kernel's layout.
type TxMetadata struct {
	Flags      uint64 // TxMetaChecksum and/or TxMetaTimestamp
	CsumStart  uint16 // offset from the start of the packet of the header to sum from
	CsumOffset uint16 // offset from CsumStart of the 16-bit checksum field
}

// DescTxMetadata is the Desc.Options bit that says the frame carries
// transmit metadata. Alloc sets it on the descriptors it hands out; a caller
// building descriptors of its own (a shared-memory frame, see
// WithSharedMemory) sets it when it has filled the metadata in.
const DescTxMetadata = uint32(unix.XDP_TX_METADATA)

// Metadata request flags.
const (
	TxMetaTimestamp = uint64(unix.XDP_TXMD_FLAGS_TIMESTAMP)
	TxMetaChecksum  = uint64(unix.XDP_TXMD_FLAGS_CHECKSUM)
)

// Put writes m into a metadata slice from TxMeta. For a checksum request the
// checksum field of the packet must already hold the un-complemented
// pseudo-header sum (Linux's CHECKSUM_PARTIAL); the NIC adds the rest.
func (m TxMetadata) Put(b []byte) {
	_ = b[txMetadataLen-1]
	binary.LittleEndian.PutUint64(b[0:8], m.Flags)
	binary.LittleEndian.PutUint16(b[8:10], m.CsumStart)
	binary.LittleEndian.PutUint16(b[10:12], m.CsumOffset)
	clear(b[12:16])
}

// WithTxMetadata reserves transmit metadata space in front of every transmit
// frame and marks every transmit descriptor as carrying it. Needs Linux 6.8+
// (the bind fails otherwise) and, for the NIC to act on a request, a driver
// that reports the feature (QueryXSKFeatures). Incompatible with
// WithTxReuseRxFrames: received frames have no gap in front of them.
func WithTxMetadata() Option { return func(c *config) { c.opts.TxMetadataLen = txMetadataLen } }

// TxMetadataLen is the size of the metadata in front of each transmit frame,
// zero when WithTxMetadata was not used.
func (xsk *Socket) TxMetadataLen() int { return xsk.options.TxMetadataLen }

// TxMeta returns the metadata slice for a transmit frame obtained from
// GetFrame or a SendFunc build callback: the TxMetadataLen bytes just before
// it in the UMEM. It returns nil when the socket has no transmit metadata.
func (xsk *Socket) TxMeta(frame []byte) []byte {
	n := xsk.options.TxMetadataLen
	if n == 0 || len(frame) == 0 {
		return nil
	}
	off := uintptr(unsafe.Pointer(&frame[0])) - uintptr(unsafe.Pointer(&xsk.umem[0]))
	return xsk.umem[off-uintptr(n) : off]
}

// payloadCap is the largest packet a transmit frame holds: the chunk minus
// the metadata in front of it.
func (xsk *Socket) payloadCap() int { return xsk.options.FrameSize - xsk.options.TxMetadataLen }
