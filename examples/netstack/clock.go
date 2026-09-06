//go:build linux

package main

import (
	"time"
	_ "unsafe" // for go:linkname

	"gvisor.dev/gvisor/pkg/tcpip"
)

// fastClock is a tcpip.Clock whose monotonic reading is one call to the
// runtime's nanotime. gVisor's own stdClock does time.Since(base) — a
// time.Now() that reads the wall clock and the monotonic clock, then a
// Sub — and TCP reads the clock several times per segment (arrival time,
// RTT, RACK, timer arming). Timers are the standard library's, as upstream.
type fastClock struct {
	base int64
}

//go:linkname nanotime runtime.nanotime
func nanotime() int64

func newFastClock() tcpip.Clock { return &fastClock{base: nanotime()} }

func (c *fastClock) Now() time.Time { return time.Now() }

func (c *fastClock) NowMonotonic() tcpip.MonotonicTime {
	return tcpip.MonotonicTime{}.Add(time.Duration(nanotime() - c.base))
}

func (c *fastClock) AfterFunc(d time.Duration, f func()) tcpip.Timer {
	return &stdTimer{t: time.AfterFunc(d, f)}
}

type stdTimer struct{ t *time.Timer }

func (t *stdTimer) Stop() bool            { return t.t.Stop() }
func (t *stdTimer) Reset(d time.Duration) { t.t.Reset(d) }
