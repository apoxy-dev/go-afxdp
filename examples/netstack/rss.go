//go:build linux

package main

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
)

// RSS, as the NIC computes it, so a client can choose source ports that land
// each connection's replies on the queue whose stack opened it.
//
// A server never needs this: RSS decides the queue before the connection
// exists and one stack per queue follows. A client picks its source port
// before it sees any reply, so with one stack per queue it must pick a port
// whose four-tuple hashes to its own queue — the receive queue of the
// reply is Toeplitz(key, src ip, dst ip, src port, dst port) of the reply,
// looked up in the indirection table. Both come from ethtool.

type rss struct {
	key   []byte
	indir []int // hash % len -> queue
}

// readRSS reads the interface's RSS key and indirection table (ethtool -x).
func readRSS(iface string) (*rss, error) {
	out, err := exec.Command("ethtool", "-x", iface).Output()
	if err != nil {
		return nil, fmt.Errorf("ethtool -x %s: %v", iface, err)
	}
	r := &rss{}
	inKey := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "RSS hash key"):
			inKey = true
		case strings.HasPrefix(line, "RSS hash function"):
			inKey = false
		case inKey && len(f) == 1 && strings.Contains(f[0], ":"):
			b, err := hex.DecodeString(strings.ReplaceAll(f[0], ":", ""))
			if err != nil {
				return nil, fmt.Errorf("RSS key: %v", err)
			}
			r.key = b
		case len(f) > 1 && strings.HasSuffix(f[0], ":") && !inKey:
			// "    0:      0     1     2 ..." indirection rows
			if _, err := strconv.Atoi(strings.TrimSuffix(f[0], ":")); err != nil {
				continue
			}
			for _, q := range f[1:] {
				n, err := strconv.Atoi(q)
				if err != nil {
					continue
				}
				r.indir = append(r.indir, n)
			}
		}
	}
	if len(r.key) < 40 || len(r.indir) == 0 {
		return nil, fmt.Errorf("could not parse RSS key (%d bytes) / table (%d entries) from ethtool -x %s", len(r.key), len(r.indir), iface)
	}
	return r, nil
}

// toeplitz hashes data with the key, as the NIC does.
func (r *rss) toeplitz(data []byte) uint32 {
	var hash uint32
	// The key is consumed as a bit string; keep a 32-bit window into it.
	window := uint32(r.key[0])<<24 | uint32(r.key[1])<<16 | uint32(r.key[2])<<8 | uint32(r.key[3])
	next := 4
	for _, b := range data {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<bit) != 0 {
				hash ^= window
			}
			window <<= 1
			if next < len(r.key) && r.key[next]&(1<<bit) != 0 {
				window |= 1
			}
			// Advance one key bit: when the byte's bits are used up, move on.
			if bit == 0 {
				next++
			}
		}
	}
	return hash
}

// queueFor is the receive queue of an IPv4 TCP packet from src:sport to
// dst:dport.
func (r *rss) queueFor(src, dst netip.Addr, sport, dport uint16) int {
	s, d := src.As4(), dst.As4()
	data := []byte{s[0], s[1], s[2], s[3], d[0], d[1], d[2], d[3],
		byte(sport >> 8), byte(sport), byte(dport >> 8), byte(dport)}
	return r.indir[int(r.toeplitz(data))%len(r.indir)]
}

// portsForQueue returns source ports in [lo, hi) whose replies (from
// peer:peerPort to local:port) land on queue q.
func (r *rss) portsForQueue(local, peer netip.Addr, peerPort uint16, q, lo, hi, want int) []uint16 {
	var ports []uint16
	for p := lo; p < hi && len(ports) < want; p++ {
		if r.queueFor(peer, local, peerPort, uint16(p)) == q {
			ports = append(ports, uint16(p))
		}
	}
	return ports
}
