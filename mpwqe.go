//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package afxdp

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The mlx5 driver's XDP transmit path has two descriptor formats, switched by
// the xdp_tx_mpwqe private flag, and they trade exactly the way the card's own
// native modes do. With the flag on (the kernel default) small packets are
// copied into multi-packet descriptors: more CPU per packet — with a 25% step
// where a packet crosses from four to five 16-byte descriptor slots, at a
// 61-byte frame — but the ceiling multiplies with queues, to line rate. With
// it off the driver posts pointer descriptors: far cheaper per core and no
// step, but the card drains them through one door, ~75 Mpps device-wide
// however many queues.
//
// Measured on a ConnectX-6 Dx at 100G, one transmit queue, one core:
//
//	frame     mpwqe on     mpwqe off
//	60 B      14.3 Mpps    17.5 Mpps
//	64 B       8.5 Mpps    17.5 Mpps
//
// and at scale: off walls at 75.3 Mpps from six queues up, on reaches 140.7
// (16 queues, 68-byte frames) to 147.9 (64-byte). So the right setting is the
// queue count's to make: four queues or fewer can only win with the flag off
// (70.2 Mpps on four cores against ~58 with it on), five or more need it on
// to pass the wall. Open applies that choice to zero-copy fleets on mlx5
// interfaces (copy-mode sockets transmit through the skb path, which the flag
// does not change), saves the original, and Close puts it back — the same contract as the NAPI tuning.
// WithoutMPWQETune (or WithoutAutoTune) leaves the interface alone.
const mpwqeOffMaxQueues = 4

// mpwqeFlagName is the mlx5 private flag that selects the XDP transmit
// descriptor format.
const mpwqeFlagName = "xdp_tx_mpwqe"

// ethtool ioctl ABI. These are frozen kernel ABI values (linux/ethtool.h),
// defined here so the tuning has no dependency beyond a socket.
const (
	siocEthtool       = 0x8946
	ethtoolGSSetInfo  = 0x00000037
	ethtoolGStrings   = 0x0000001b
	ethtoolGPFlags    = 0x00000027
	ethtoolSPFlags    = 0x00000028
	ethSSPrivFlags    = 2
	ethGStringLen     = 32
	ethtoolMaxStrings = 32 // private flag sets are tiny; cap the buffer
)

// mpwqeTuning remembers the interface's original xdp_tx_mpwqe state so Close
// can put it back. Like the NAPI settings, the flag belongs to the host: it
// outlives the process and affects every XDP transmitter on the interface,
// which is why it is saved, refcounted per interface, restored on Close, and
// reported through Fleet.Info rather than changed silently.
type mpwqeTuning struct {
	iface string
	bit   uint32
	was   bool
	desc  string
}

var (
	mpwqeMu   sync.Mutex
	mpwqeRefs = make(map[string]*mpwqeRef)
)

type mpwqeRef struct {
	tuning *mpwqeTuning
	n      int
}

// applyMPWQETuning turns xdp_tx_mpwqe off for a fleet of nQueues queues when
// that is the measured win, returning a handle that restores the original
// state. Entirely best-effort: on a non-mlx5 driver, a kernel without the
// flag, or without privileges, it returns nil and the caller carries on.
//
// forceOff turns the flag off whatever the queue count. Transmit metadata
// needs it: with xdp_tx_mpwqe on, mlx5 (7.0) loses the checksum request of
// a few packets in a thousand — the frames go out with the pseudo-header
// partial in the checksum field and the peer drops them — while with the
// flag off every request is honoured (measured on ConnectX-6, fw 22.40).
func applyMPWQETuning(iface string, nQueues int, forceOff bool) *mpwqeTuning {
	if nQueues > mpwqeOffMaxQueues && !forceOff {
		return nil // the copy path is the right one; the kernel default stands
	}
	if !strings.HasPrefix(interfaceDriver(iface), "mlx5") {
		return nil
	}

	mpwqeMu.Lock()
	defer mpwqeMu.Unlock()
	if ref, ok := mpwqeRefs[iface]; ok {
		ref.n++ // the first fleet's choice stands for the interface
		return ref.tuning
	}

	bit, err := privFlagBit(iface, mpwqeFlagName)
	if err != nil {
		return nil
	}
	flags, err := getPrivFlags(iface)
	if err != nil {
		return nil
	}
	was := flags&(1<<bit) != 0
	if was {
		if err := setPrivFlags(iface, flags&^(1<<bit)); err != nil {
			return nil
		}
	}
	t := &mpwqeTuning{
		iface: iface,
		bit:   bit,
		was:   was,
		desc:  fmt.Sprintf("%s=off (%d queue(s); pointer descriptors win to %d)", mpwqeFlagName, nQueues, mpwqeOffMaxQueues),
	}
	if forceOff {
		t.desc = fmt.Sprintf("%s=off (transmit metadata: the packed path loses checksum requests)", mpwqeFlagName)
	}
	mpwqeRefs[iface] = &mpwqeRef{tuning: t, n: 1}
	return t
}

// restore puts the interface's original flag state back once the last fleet
// using it has closed. Safe on a nil receiver.
func (t *mpwqeTuning) restore() {
	if t == nil {
		return
	}
	mpwqeMu.Lock()
	defer mpwqeMu.Unlock()
	ref, ok := mpwqeRefs[t.iface]
	if !ok {
		return
	}
	if ref.n--; ref.n > 0 {
		return
	}
	delete(mpwqeRefs, t.iface)
	if !t.was {
		return // it was already off; nothing was changed
	}
	if flags, err := getPrivFlags(t.iface); err == nil {
		_ = setPrivFlags(t.iface, flags|(1<<t.bit))
	}
}

// String renders the tuning for Fleet.Info. Nil means untouched.
func (t *mpwqeTuning) String() string {
	if t == nil {
		return ""
	}
	return t.desc
}

// ---------------------------------------------------------------- ethtool

// ethtoolIoctl runs one SIOCETHTOOL request for iface with arg pointing at an
// ethtool command struct.
func ethtoolIoctl(iface string, arg unsafe.Pointer) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	// struct ifreq: 16 bytes of name, then the data pointer.
	var ifr [40]byte
	if len(iface) >= 16 {
		return fmt.Errorf("afxdp: interface name %q too long", iface)
	}
	copy(ifr[:], iface)
	*(*uintptr)(unsafe.Pointer(&ifr[16])) = uintptr(arg)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), siocEthtool, uintptr(unsafe.Pointer(&ifr[0])))
	runtime.KeepAlive(arg)
	if errno != 0 {
		return errno
	}
	return nil
}

// privFlagBit finds the bit index of a named private flag.
func privFlagBit(iface, name string) (uint32, error) {
	// struct ethtool_sset_info { u32 cmd; u32 reserved; u64 sset_mask; u32 data[1]; }
	var sset struct {
		cmd      uint32
		reserved uint32
		ssetMask uint64
		data     uint32
	}
	sset.cmd = ethtoolGSSetInfo
	sset.ssetMask = 1 << ethSSPrivFlags
	if err := ethtoolIoctl(iface, unsafe.Pointer(&sset)); err != nil {
		return 0, err
	}
	count := sset.data
	if count == 0 || count > ethtoolMaxStrings {
		return 0, fmt.Errorf("afxdp: %s has no private flags", iface)
	}

	// struct ethtool_gstrings { u32 cmd; u32 string_set; u32 len; u8 data[]; }
	var gs struct {
		cmd       uint32
		stringSet uint32
		length    uint32
		data      [ethtoolMaxStrings * ethGStringLen]byte
	}
	gs.cmd = ethtoolGStrings
	gs.stringSet = ethSSPrivFlags
	gs.length = count
	if err := ethtoolIoctl(iface, unsafe.Pointer(&gs)); err != nil {
		return 0, err
	}
	for i := uint32(0); i < count; i++ {
		s := gs.data[i*ethGStringLen : (i+1)*ethGStringLen]
		if strings.TrimRight(string(s), "\x00") == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("afxdp: %s has no private flag %q", iface, name)
}

func getPrivFlags(iface string) (uint32, error) {
	v := struct{ cmd, data uint32 }{cmd: ethtoolGPFlags}
	if err := ethtoolIoctl(iface, unsafe.Pointer(&v)); err != nil {
		return 0, err
	}
	return v.data, nil
}

func setPrivFlags(iface string, flags uint32) error {
	v := struct{ cmd, data uint32 }{cmd: ethtoolSPFlags, data: flags}
	return ethtoolIoctl(iface, unsafe.Pointer(&v))
}
