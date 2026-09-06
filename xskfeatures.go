//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package afxdp

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// XSKFeatures is what a device reports about its XDP and AF_XDP support,
// from the kernel's netdev generic-netlink family (Linux 6.3+; the transmit
// metadata bits need 6.8+).
type XSKFeatures struct {
	XDPFeatures uint64 // NETDEV_XDP_ACT_* bits: BASIC 1, REDIRECT 2, NDO_XMIT 4, XSK_ZEROCOPY 8, HW_OFFLOAD 16, RX_SG 32, NDO_XMIT_SG 64
	ZCMaxSegs   uint32 // largest multi-buffer packet, in frames, a zero-copy socket may receive (1 = none)
	RXMetadata  uint64 // NETDEV_XDP_RX_METADATA_* bits: TIMESTAMP 1, HASH 2, VLAN_TAG 4
	TxTimestamp bool   // the driver honours TxMetaTimestamp on transmit
	TxChecksum  bool   // the driver honours TxMetaChecksum on transmit
}

// ZeroCopy reports whether the device offers zero-copy AF_XDP at all.
func (f XSKFeatures) ZeroCopy() bool { return f.XDPFeatures&8 != 0 }

const (
	netdevCmdDevGet             = 1
	netdevAttrIfindex           = 1
	netdevAttrXDPFeatures       = 3
	netdevAttrXDPZCMaxSegs      = 4
	netdevAttrXDPRXMetaFeatures = 5
	netdevAttrXSKFeatures       = 6
	netdevXSKTxTimestamp        = 1
	netdevXSKTxChecksum         = 2
)

// genlHeader is struct genlmsghdr. (netlink's nl.Genlmsg is a two-byte struct
// whose Serialize reads four bytes, so its reserved field is whatever follows
// it in memory; the netdev family validates strictly and rejects the request
// when that is not zero.)
type genlHeader struct {
	cmd, version uint8
}

func (genlHeader) Len() int { return nl.SizeofGenlmsg }
func (h genlHeader) Serialize() []byte {
	return []byte{h.cmd, h.version, 0, 0}
}

// QueryXSKFeatures asks the kernel what iface can do. It is the only way to
// learn, before sending, whether a transmit checksum request will be
// honoured: a driver that ignores it transmits the frame silently as is.
func QueryXSKFeatures(iface string) (XSKFeatures, error) {
	var f XSKFeatures
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return f, err
	}
	fam, err := netlink.GenlFamilyGet("netdev")
	if err != nil {
		return f, fmt.Errorf("afxdp: netdev netlink family (needs Linux 6.3+): %w", err)
	}
	req := nl.NewNetlinkRequest(int(fam.ID), unix.NLM_F_REQUEST)
	req.AddData(genlHeader{cmd: netdevCmdDevGet, version: 1})
	req.AddData(nl.NewRtAttr(netdevAttrIfindex, nl.Uint32Attr(uint32(ifi.Index))))
	msgs, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return f, fmt.Errorf("afxdp: netdev dev-get %s: %w", iface, err)
	}
	for _, m := range msgs {
		if len(m) < nl.SizeofGenlmsg {
			continue
		}
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return f, err
		}
		for _, a := range attrs {
			// The kernel is trusted, but a short attribute must not panic
			// a root process: skip anything narrower than its type says.
			switch a.Attr.Type {
			case netdevAttrXDPFeatures:
				if len(a.Value) >= 8 {
					f.XDPFeatures = binary.LittleEndian.Uint64(a.Value)
				}
			case netdevAttrXDPZCMaxSegs:
				if len(a.Value) >= 4 {
					f.ZCMaxSegs = binary.LittleEndian.Uint32(a.Value)
				}
			case netdevAttrXDPRXMetaFeatures:
				if len(a.Value) >= 8 {
					f.RXMetadata = binary.LittleEndian.Uint64(a.Value)
				}
			case netdevAttrXSKFeatures:
				if len(a.Value) >= 8 {
					v := binary.LittleEndian.Uint64(a.Value)
					f.TxTimestamp = v&netdevXSKTxTimestamp != 0
					f.TxChecksum = v&netdevXSKTxChecksum != 0
				}
			}
		}
	}
	return f, nil
}
