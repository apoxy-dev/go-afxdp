//go:build linux && gvisorfork

package main

import (
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// externalWriter is the fork's addition to the TCP endpoint.
type externalWriter interface {
	WriteExternal(vs []*buffer.View) (int64, tcpip.Error)
}

// sourceBatch is how many cache frames go to TCP in one write: a GSO
// packet's worth, so the send path does its per-segment work once per 64 KiB
// as it does for the copying source.
const sourceBatch = 45

// serveSource streams the cache to the peer without copying it: each cache
// frame's payload goes to TCP as an external view (the fork's WriteExternal),
// and the link endpoint transmits the frame in place. It runs on the receive
// goroutine, from the writable event, until the send buffer is full.
func (c *rtcConn) serveSource() {
	cache := c.srv.source
	n := cachePayload(cache)
	for {
		if len(c.held) == 0 {
			// One strided view per batch: the stack sees count*n bytes, the
			// endpoint sees count frames.
			count := sourceBatch
			if left := cache.Frames() - c.next; left < count {
				count = left
			}
			c.held = append(c.held, buffer.NewViewFromExternalSized(cache.Span(c.next, count), count*n, nil))
			c.next += count
			if c.next >= cache.Frames() {
				c.next = 0
			}
		}
		wrote, terr := c.ep.(externalWriter).WriteExternal(c.held)
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
				return // the writable event brings us back; c.held is still ours
			}
			for _, v := range c.held {
				v.Release()
			}
			c.held = c.held[:0]
			c.close()
			return
		}
		c.srv.srv.bytesOut.Add(uint64(wrote))
		c.held = c.held[:0] // the endpoint owns them now
	}
}
