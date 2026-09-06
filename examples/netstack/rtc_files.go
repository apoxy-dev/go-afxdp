//go:build linux && gvisorfork

package main

import (
	"bytes"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// serveFilesRTC is the file server on the run-to-completion path: the
// receive goroutine that delivered a connection's request reads it, finds
// the object in the cache, and queues its frames to TCP as external views —
// no goroutine per connection, no copy into the stack, no copy into frames.
// A full send buffer returns WouldBlock; the writable event brings the
// connection back with c.held still owed.
func (c *rtcConn) serveFilesRTC(buf []byte) {
	f := c.srv.files
	for {
		// Owed frames first.
		if len(c.held) > 0 {
			wrote, terr := c.ep.(externalWriter).WriteExternal(c.held)
			if terr != nil {
				if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
					return
				}
				for _, v := range c.held {
					v.Release()
				}
				c.held = c.held[:0]
				c.close()
				return
			}
			c.srv.srv.bytesOut.Add(uint64(wrote))
			c.held = c.held[:0]
		}
		// More of the object in progress?
		if c.objLeft > 0 {
			count := min(sourceBatch, c.objLeft)
			size := count * f.plen
			if c.objBytes < size {
				size = c.objBytes
			}
			c.held = append(c.held, buffer.NewViewFromExternalSized(f.cache.Span(c.objNext, count), size, nil))
			c.objNext += count
			c.objLeft -= count
			c.objBytes -= size
			continue
		}
		// Next request line, if one has arrived.
		if i := bytes.IndexByte(c.req, '\n'); i >= 0 {
			name := string(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(c.req[:i]), []byte("GET /"))))
			c.req = c.req[:copy(c.req, c.req[i+1:])]
			idx, ok := f.byIndex[name]
			if !ok {
				c.close()
				return
			}
			c.srv.srv.bytesIn.Add(uint64(i + 1))
			c.objNext, c.objLeft = f.object(idx, c.replica)
			c.objBytes = f.classes[idx].bytes
			continue
		}
		w := tcpip.SliceWriter(buf)
		res, terr := c.ep.Read(&w, tcpip.ReadOptions{})
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
				c.close()
			}
			return
		}
		if len(c.req)+res.Count > 4096 {
			c.close() // no request line that long
			return
		}
		c.req = append(c.req, buf[:res.Count]...)
	}
}
