//go:build linux && gvisorfork

package main

import (
	"log"
	"os"
	"testing"
	"time"

	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
)

// TestVethBackpressure runs the veth echo with a two-packet transmit backlog:
// the stack is refused constantly and must pause and resume rather than
// lose data. The echo checks every byte; here the refusals must have
// happened and the loss recovery must not have.
func TestVethBackpressure(t *testing.T) {
	// A veth never fills the ring, so the in-flight cap is what makes a
	// backlog here: one 45-frame packet in flight, two waiting, then
	// refusals.
	old1, old2, old3 := txBacklogHigh, txBacklogLow, txInflightFlag
	txBacklogHigh, txBacklogLow, txInflightFlag = 2, 1, 64
	t.Cleanup(func() { txBacklogHigh, txBacklogLow, txInflightFlag = old1, old2, old3 })
	applyForkFlags() // main does this; the fork's switch must be on for the test
	if os.Getenv("BP_DEBUG") != "" {
		vethOnStart = func(rt *netstackRuntime) {
			go func() {
				for range time.Tick(500 * time.Millisecond) {
					es := rt.ep.Stats()
					ts := rt.s.Stats().TCP
					log.Printf("bp: refused %d resumed %d tx %d txDeferred %d | segs %d retx %d timeouts %d | %s", es.TxRefused, es.TxResumed, es.Tx, es.TxDeferred, ts.SegmentsSent.Value(), ts.Retransmits.Value(), ts.Timeouts.Value(), rt.ep.DebugTx(0))
				}
			}()
		}
		t.Cleanup(func() { vethOnStart = nil })
	}
	// Direct transmit: under the fifo qdisc the refusal never reaches the
	// sender.
	vethEchoWithCheck(t, false, xdplink.TxDirect, func(t *testing.T, rt *netstackRuntime) {
		es := rt.ep.Stats()
		if es.TxRefused == 0 {
			t.Fatalf("the backlog limit never refused a write: %+v", es)
		}
		if es.TxDropped != 0 {
			t.Errorf("packets dropped despite backpressure: %d", es.TxDropped)
		}
		if n := rt.s.Stats().TCP.Retransmits.Value(); n > 5 {
			t.Errorf("%d retransmits: refusals were treated as loss", n)
		}
		ts := rt.s.Stats().TCP
		t.Logf("refused %d writes, resumed %d; retransmits %d timeouts %d fastRetx %d sackRecovery %d", es.TxRefused, es.TxResumed, ts.Retransmits.Value(), ts.Timeouts.Value(), ts.FastRetransmit.Value(), ts.SACKRecovery.Value())
	})
}
