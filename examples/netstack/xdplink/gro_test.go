//go:build linux

package xdplink

import (
	"sync/atomic"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/stack/gro"
)

// fakeDispatcher records what reaches the stack.
type fakeDispatcher struct {
	pkts  int
	bytes int
}

func (f *fakeDispatcher) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	f.pkts++
	f.bytes += pkt.Data().Size()
}
func (f *fakeDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

// wireSegment builds a VLAN-tagged IPv4/TCP frame as a Linux peer would send
// it in a bulk stream: full checksums, a timestamp option, PSH only if asked.
func wireSegment(seq uint32, payload []byte, psh bool) []byte {
	const tcpLen = header.TCPMinimumSize + 12
	f := make([]byte, vlanHdrLen+header.IPv4MinimumSize+tcpLen+len(payload))
	putHeader(f, srcMAC, dstMAC, testVLAN, etherTypeIPv4)
	ip := header.IPv4(f[vlanHdrLen:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + tcpLen + len(payload)), TTL: 64,
		Protocol: uint8(header.TCPProtocolNumber), SrcAddr: testDst, DstAddr: testSrc,
		Flags: header.IPv4FlagDontFragment,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	tcp := header.TCP(f[vlanHdrLen+header.IPv4MinimumSize:])
	flags := header.TCPFlagAck
	if psh {
		flags |= header.TCPFlagPsh
	}
	tcp.Encode(&header.TCPFields{SrcPort: 43210, DstPort: 8080, SeqNum: seq, AckNum: 77,
		DataOffset: tcpLen, Flags: flags, WindowSize: 500})
	copy(tcp[header.TCPMinimumSize:], []byte{1, 1, 8, 10, 0, 0, 0, 9, 0, 0, 0, 3})
	copy(tcp[tcpLen:], payload)
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, testDst, testSrc, uint16(tcpLen+len(payload)))
	xsum = checksum.Checksum(payload, xsum)
	tcp.SetChecksum(^tcp.CalculateChecksum(xsum))
	return f
}

func TestGROMergesABurst(t *testing.T) {
	e := &Endpoint{cfg: Config{VLAN: testVLAN, GRO: true}, hdrLen: vlanHdrLen, neigh: newNeighbours()}
	copy(e.mac[:], srcMAC[:])
	var delivered atomic.Uint64
	fd := &fakeDispatcher{}
	g := &gro.GRO{Dispatcher: countingDispatcher{d: fd, n: &delivered}}
	g.Init(true)

	payload := make([]byte, testMSS)
	const n = 10
	for i := 0; i < n; i++ {
		e.deliver(0, wireSegment(uint32(1+i*testMSS), payload, i == n-1), fd, g)
	}
	g.Flush()

	if fd.pkts != 1 {
		t.Fatalf("GRO delivered %d packets for %d contiguous segments, want 1 (merge ratio broken)", fd.pkts, n)
	}
	wantBytes := header.IPv4MinimumSize + header.TCPMinimumSize + 12 + n*testMSS
	if fd.bytes != wantBytes {
		t.Fatalf("merged packet is %d bytes, want %d", fd.bytes, wantBytes)
	}
}
