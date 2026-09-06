//go:build linux

package main

import (
	"bytes"
	"errors"
	"math/rand"
	"strconv"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The file-fetch load generator on netstack (-client -files -client-stack
// netstack).
//
// The kernel client is the ceiling on the connection-churn rows: it spends
// about 1.4 ms of CPU per connection against the server's 40 µs, so at 30k
// connections a second it is flat out on 48 threads and the server, on four
// cores, is half idle. What those rows then measure is the peer.
//
// This drives the same workload from the run-to-completion path: no
// goroutine per connection, one small state machine each — ask for an
// object, read exactly its bytes, ask for the next — serviced by the
// receive goroutine that delivered the data. After -per-conn objects the
// connection is closed and dialled again from the same stack with the next
// source port in that queue's RSS list, so the replies keep landing on the
// queue that owns it.

// filesVerify makes the client check every byte it receives against the
// object's pattern. Tests set it; a load generator does not, because the
// comparison is as expensive as the stack it is measuring.
var filesVerify = false

// filesState is one connection's place in the workload.
type filesState struct {
	rnd      *rand.Rand
	idx      int       // class of the object in flight
	left     int       // bytes of it still to come
	start    time.Time // when its request went out
	first    bool      // its first byte is still to come
	done     int       // objects finished on this connection
	dialPort uint16    // the source port this connection used
	// pausedUntil, with -slow, is when this connection may read again; a
	// timer re-queues it then. Reading stops meanwhile, the receive buffer
	// fills, the window closes, and the server sees a slow reader.
	pausedUntil int64
	// redialWaits counts consecutive dials that found every port it tried
	// still in TIME-WAIT; the connection waits and tries again rather than
	// dying, up to redialWaitMax.
	redialWaits int
	awaitRedial bool
}

// redialWait is how long a connection waits when the ports it was given
// are all still in TIME-WAIT, and redialWaitMax how many times in a row it
// will, before that counts as an error. A queue whose few early connections
// churn faster than its port cycle clears (a ramped start on an idle
// server, hundreds of dials a second each) otherwise loses them all at the
// first wrap; waiting a TIME-WAIT is the right pace.
const (
	redialWait    = 50 * time.Millisecond
	redialWaitMax = 100
)

// startObject picks the next object and asks for it.
func (c *clientConn) startObject() {
	f, fs := c.cs.files, c.fst
	c.fl.idx = f.pick[c.fl.rnd.Intn(len(f.pick))]
	class := f.classes[c.fl.idx]
	c.fl.left, c.fl.first, c.fl.start = class.bytes, true, time.Now()
	req := append(append(c.req[:0], "GET /"...), class.name...)
	c.req = append(req, '\n')
	if _, terr := c.ep.Write(bytes.NewReader(c.req), tcpip.WriteOptions{}); terr != nil {
		if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
			fs.fail("write", errors.New(terr.String()))
			c.close()
		}
	}
}

// serviceFiles reads what has arrived of the object in flight, counts it,
// and moves on: the next object, or a new connection.
func (c *clientConn) serviceFiles(buf []byte) {
	fs := c.fst
	if c.fl.awaitRedial {
		// Waiting out TIME-WAIT on the ports; the old endpoint is closed,
		// so this comes before anything that would look at it.
		c.fl.awaitRedial = false
		c.redial()
		return
	}
	if !c.connected {
		if tcp.EndpointState(c.ep.State()) != tcp.StateEstablished {
			if terr := c.ep.LastError(); terr != nil {
				fs.fail("connect", errors.New(terr.String()))
				c.redial()
			}
			return // still connecting; the next event says more
		}
		c.connected = true
		fs.conns.Add(1)
		c.startObject()
	}
	if c.fl.pausedUntil != 0 && nanotime() < c.fl.pausedUntil {
		return // the pacing timer brings us back
	}
	c.fl.pausedUntil = 0
	if slow := c.cs.fcfg.slow; slow > 0 && len(buf) > slow/4 && slow/4 >= 1500 {
		// Read a quarter-second of the pace at a time: each pause is a
		// timer, and 20000 connections pausing ten times a second each
		// cost this client eight cores; four times is enough of a slow
		// reader for the server to see one.
		buf = buf[:slow/4]
	}
	for {
		w := tcpip.SliceWriter(buf)
		res, terr := c.ep.Read(&w, tcpip.ReadOptions{})
		if terr != nil {
			if _, ok := terr.(*tcpip.ErrWouldBlock); ok {
				return
			}
			if !c.cs.stopping.Load() {
				fs.fail("read", errors.New(terr.String()))
			}
			c.redial()
			return
		}
		cs := &fs.perClass[c.fl.idx]
		if c.fl.first {
			cs.ttfb.record(time.Since(c.fl.start))
			c.fl.first = false
		}
		if filesVerify {
			off := c.cs.files.classes[c.fl.idx].bytes - c.fl.left
			if want := c.cs.files.body[off : off+res.Count]; !bytes.Equal(buf[:res.Count], want) {
				fs.fail("verify", errors.New("object bytes differ at offset "+itoa(off)))
				c.redial()
				return
			}
		}
		c.fl.left -= res.Count
		fs.bytes.Add(uint64(res.Count))
		cs.bytes.Add(uint64(res.Count))
		if c.fl.left > 0 {
			if slow := c.cs.fcfg.slow; slow > 0 {
				// Behind the pace, keep reading; ahead of it, stop until
				// the object should have got this far.
				got := c.cs.files.classes[c.fl.idx].bytes - c.fl.left
				want := time.Duration(float64(got) / float64(slow) * float64(time.Second))
				if d := want - time.Since(c.fl.start); d > 0 {
					c.fl.pausedUntil = nanotime() + int64(d)
					time.AfterFunc(d, func() { c.notify(waiter.ReadableEvents) })
					return
				}
			}
			continue
		}
		cs.hist.record(time.Since(c.fl.start))
		cs.count.Add(1)
		fs.reqs.Add(1)
		c.fl.done++
		if n := c.cs.fcfg.perConn; n > 0 && c.fl.done >= n {
			c.redial() // this connection has had its objects
			return
		}
		c.startObject()
	}
}

// redial closes this connection and starts another on the same stack, from
// the next source port in that queue's list. The clientConn itself is kept,
// so its place in the ready list stays valid.
func (c *clientConn) redial() {
	if c.fl.redialWaits == 0 {
		c.closeEndpoint() // a retry after a wait has closed it already
	}
	if c.cs.stopping.Load() {
		c.closed = true
		return
	}
	ep, wq, port, err := c.cs.newEndpoint(c.qi)
	if err != nil {
		if c.fl.redialWaits++; c.fl.redialWaits > redialWaitMax {
			c.fst.fail("connect", err)
			c.closed = true
			return
		}
		// Every port tried is still in TIME-WAIT: wait one out and try
		// again, from this connection's place in the ready list.
		c.fl.awaitRedial = true
		time.AfterFunc(redialWait, func() { c.notify(waiter.WritableEvents) })
		return
	}
	c.fl.redialWaits = 0
	if terr := ep.Connect(c.cs.target); terr != nil {
		if _, ok := terr.(*tcpip.ErrConnectStarted); !ok {
			ep.Close()
			c.fst.fail("connect", errors.New(terr.String()))
			c.closed = true
			return
		}
	}
	c.ep, c.wq, c.connected, c.epClosed = ep, wq, false, false
	c.fl.done, c.fl.dialPort = 0, port
	c.entry = waiter.NewFunctionEntry(waiter.ReadableEvents|waiter.WritableEvents|waiter.EventHUp|waiter.EventErr, c.notify)
	wq.EventRegister(&c.entry)
	c.notify(waiter.WritableEvents) // in case the handshake beat the registration
}

// newEndpoint makes and binds one endpoint on stack qi, taking the next
// source port in that queue's cycle (see rss.go); with no RSS table the port
// choice is left to the stack. The caller registers its waiter, then connects.
func (cs *clientSet) newEndpoint(qi int) (tcpip.Endpoint, *waiter.Queue, uint16, error) {
	wq := &waiter.Queue{}
	s := cs.stacks[qi]
	ep, terr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, wq)
	if terr != nil {
		return nil, nil, 0, errors.New(terr.String())
	}
	ep.SocketOptions().SetDelayOption(false)
	if cs.fcfg.slow > 0 {
		ep.SocketOptions().SetReceiveBufferSize(64<<10, true)
	}
	var port uint16
	if ports := cs.ports[qi]; len(ports) > 0 {
		// Take the next port in the cycle, stepping over any the stack
		// still holds: a connection of ours, or one in TIME-WAIT. When
		// none of a run of them can be had, the cursor goes back to where
		// it started, so the retry after a wait tries the oldest ports —
		// the ones about to leave TIME-WAIT — not ever fresher ones.
		cs.portMu.Lock()
		start := cs.portAt[qi]
		var terr tcpip.Error
		for try := 0; try < 16; try++ {
			port = ports[cs.portAt[qi]%len(ports)]
			cs.portAt[qi]++
			if terr = ep.Bind(tcpip.FullAddress{Addr: cs.local, Port: port}); terr == nil {
				break
			}
		}
		if terr != nil {
			cs.portAt[qi] = start
		}
		cs.portMu.Unlock()
		if terr != nil {
			ep.Close()
			return nil, nil, 0, errors.New(terr.String())
		}
	}
	return ep, wq, port, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
