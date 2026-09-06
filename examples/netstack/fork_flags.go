//go:build linux && gvisorfork

package main

import (
	"flag"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// Knobs that exist only in the patched gVisor fork (see ./patches). Build with
// -tags gvisorfork against the fork (go.work points at it) to get them; the
// default build compiles against upstream gVisor and has none of this.

var (
	reorderWindow = flag.Duration("reorder-window", 5*time.Millisecond,
		"floor on the RACK reorder window (0 = upstream behaviour); 5ms measured best on a 10G LAN")
	reorderNoDupthresh = flag.Bool("reorder-nodupthresh", true,
		"do not zero the RACK reorder window at 3 SACKed segments")
	inlineTCP = flag.Bool("inline-tcp", true,
		"process an uncontended connection's inbound segments on the receive goroutine instead of waking a TCP processor (saves a wakeup per request)")
	txHashRx = flag.Bool("txhash-rx", true,
		"transmit a connection on the queue its segments arrive on (needs -inline-tcp); pairs with -deferkicks")
	batchTCP = flag.Bool("batch-tcp", true,
		"run the TCP work the inline path cannot (SYN, handshake, FIN, close) on the receive goroutine after each batch instead of waking a processor goroutine")
	linkBackpressure = flag.Bool("link-backpressure", true,
		"treat the link's 'no room' as 'not sent, try again when told' instead of a loss to recover by timer")
	linkTurn = flag.Int("link-turn", 1,
		"segments a connection the link refused may send per turn when the link resumes it (0 = its whole window)")
	minPTO = flag.Duration("min-pto", 10*time.Millisecond,
		"floor of the tail loss probe timeout (upstream: none, so twice a sub-millisecond RTT)")
	tlpWindowLimited = flag.Bool("tlp-skip-window-limited", true,
		"do not arm a tail loss probe while only the peer's window holds the sender back (such a peer has the data and has not read it)")
	zwpSeparate = flag.Bool("zwp-separate", true,
		"a zero-window probe is not a loss: send it on the retransmit timer without counting a timeout or collapsing the window")
	preciseRTT = flag.Bool("precise-rtt", true,
		"measure the round trip from the local clock even when the peer echoes a TCP timestamp (that option carries only milliseconds)")
	tcpProcessors = flag.Int("tcp-processors", 1,
		"TCP processor goroutines per stack (0 = GOMAXPROCS); with inline processing one fallback per stack is enough")
	chunkClear = flag.Bool("chunk-clear", false,
		"zero every pooled buffer chunk before reuse, as upstream does (a second pass over every received byte; nothing reads a chunk before writing it)")
	segClone = flag.Bool("seg-clone", false,
		"clone the delivered packet into each TCP segment, as upstream does (off: the segment shares it)")
	delayedACK = flag.Duration("delayed-ack", time.Millisecond,
		"hold a pure ACK this long when less than 2 MSS is unacknowledged, so a reply can carry it (0 = upstream behaviour: ACK every batch). With -mode echo-rtc the reply always beats this timer, so it costs nothing and halves the frames and kicks per request")
)

func applyForkFlags() {
	tcp.MinReorderWindow = *reorderWindow
	tcp.ReorderWindowDupthreshOverride = *reorderNoDupthresh
	tcp.DelayedACKTimeout = *delayedACK
	tcp.InlineProcessing = *inlineTCP
	tcp.TxHashFromRx = *txHashRx
	tcp.ProcessorCount = *tcpProcessors
	tcp.PreciseRTT = *preciseRTT
	tcp.BatchProcessing = *batchTCP
	tcp.LinkBackpressure = *linkBackpressure
	tcp.LinkTurn = *linkTurn
	tcpip.TrackDanglingEndpoints = false // the sentry's save-time registry: a global mutex per close
	tcp.MinPTO = *minPTO
	tcp.TLPSkipWindowLimited = *tlpWindowLimited
	tcp.SeparateZeroWindowProbe = *zwpSeparate
	buffer.ZeroOnReuse = *chunkClear
	tcp.ClonePacketForSegment = *segClone
}

// forkTxHashFromRx reports whether connections transmit on their receive queue.
func forkTxHashFromRx() bool { return *txHashRx && *inlineTCP }

// forkOutOfOrder is the fork's count of segments queued for reassembly.
func forkOutOfOrder() uint64 { return tcp.OutOfOrderSegments.Load() }

// forkDrainDeferred runs the TCP work set aside for after the batch.
func forkDrainDeferred(s *stack.Stack) { tcp.DrainDeferred(s) }

// forkLinkBackpressure reports whether the stack understands a refused write.
func forkLinkBackpressure() bool { return *linkBackpressure }

// forkResumeBlocked lets endpoints the link refused send again.
func forkResumeBlocked(s *stack.Stack) { tcp.ResumeBlocked(s) }

// forkProbeCounts reports tail loss probes fired and RACK loss marks.
func forkProbeCounts() (tlp, rackLost uint64) { return tcp.TLPProbes.Load(), tcp.RACKLossMarks.Load() }
