//go:build linux

package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The load generator. It uses kernel sockets and is meant to run on another
// host, pointed at the server under test, in one of three shapes:
//
//   - request/response (default): every connection sends -msg bytes, waits
//     for the echo, repeats. Measures requests per second and latency.
//   - -reconnect: a new connection for every request. Measures connections
//     per second, the SYN/accept/FIN path.
//   - -bulk: every connection streams 64 KiB writes while draining the echo.
//     Measures bytes per second.
//
// Against -mode http use wrk instead; this speaks echo only.

type clientConfig struct {
	target    string
	conns     int
	msg       int
	duration  time.Duration
	bulk      bool
	reconnect bool
	sinkOnly  bool
	recvOnly  bool
	// files, when set, makes this the file-fetch load generator on the
	// run-to-completion path (client_files.go).
	files    *fileSet
	filesCfg *filesConfig
}

type clientStats struct {
	conns, reqs, bytes, errs atomic.Uint64
	hist                     latencyHist
	el                       errLog
	elMu                     sync.Mutex
}

// fail counts an error and logs it, at most one line per second, so the
// first failure of a run is named rather than just counted.
func (st *clientStats) fail(what string, err error) {
	st.errs.Add(1)
	st.elMu.Lock()
	st.el.printf("%s: %v", what, err)
	st.elMu.Unlock()
}

func runClient(cfg clientConfig) {
	if cfg.conns <= 0 || cfg.msg <= 0 {
		log.Fatalf("-conns and -msg must be positive")
	}
	if cfg.bulk && cfg.reconnect {
		log.Fatalf("-bulk and -reconnect are exclusive")
	}
	log.Printf("client: %s, %d connections, %s", cfg.target, cfg.conns, describe(cfg))

	var st clientStats
	deadline := time.Now().Add(cfg.duration)
	var wg sync.WaitGroup
	for i := 0; i < cfg.conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch {
			case cfg.bulk:
				bulkWorker(cfg, &st, deadline)
			case cfg.reconnect:
				reconnectWorker(cfg, &st, deadline)
			default:
				persistentWorker(cfg, &st, deadline)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	reportLoop(cfg, &st, done)
}

// reportLoop prints one line a second and the totals when done closes.
func reportLoop(cfg clientConfig, st *clientStats, done <-chan struct{}) {
	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last struct{ conns, reqs, bytes uint64 }
	// The totals are taken at the deadline, not when the last worker has
	// closed: a close can take seconds against a sender that keeps the
	// receive buffer full, and that time is not traffic.
	var at struct {
		el      float64
		c, r, b uint64
	}
	snap := time.AfterFunc(cfg.duration, func() {
		at.el = time.Since(start).Seconds()
		at.c, at.r, at.b = st.conns.Load(), st.reqs.Load(), st.bytes.Load()
	})
	defer snap.Stop()
	for {
		select {
		case <-tick.C:
			c, r, b := st.conns.Load(), st.reqs.Load(), st.bytes.Load()
			log.Printf("conns %d/s  reqs %d/s  %s  errs %d",
				c-last.conns, r-last.reqs, gbps(b-last.bytes), st.errs.Load())
			last.conns, last.reqs, last.bytes = c, r, b
		case <-done:
			el := time.Since(start).Seconds()
			c, r, b := st.conns.Load(), st.reqs.Load(), st.bytes.Load()
			if at.el > 0 {
				el, c, r, b = at.el, at.c, at.r, at.b
			}
			log.Printf("total over %.1fs: %.0f conn/s  %.0f req/s  %s  (%d conns, %d reqs, %d errors)",
				el, float64(c)/el, float64(r)/el, gbps(uint64(float64(b)/el)), c, r, st.errs.Load())
			if !cfg.bulk {
				pct := st.hist.percentiles(0.5, 0.99)
				log.Printf("latency p50 %s  p99 %s", pct[0], pct[1])
			}
			return
		}
	}
}

func describe(cfg clientConfig) string {
	switch {
	case cfg.files != nil:
		return fmt.Sprintf("objects %s, %d per connection, for %s", cfg.filesCfg.mix, cfg.filesCfg.perConn, cfg.duration)
	case cfg.bulk:
		return fmt.Sprintf("bulk 64 KiB writes for %s", cfg.duration)
	case cfg.reconnect:
		return fmt.Sprintf("one %d-byte request per connection for %s", cfg.msg, cfg.duration)
	default:
		return fmt.Sprintf("%d-byte request/response for %s", cfg.msg, cfg.duration)
	}
}

func dial(cfg clientConfig, st *clientStats, deadline time.Time) net.Conn {
	c, err := net.DialTimeout("tcp", cfg.target, 5*time.Second)
	if err != nil {
		st.fail("dial", err)
		return nil
	}
	st.conns.Add(1)
	c.SetDeadline(deadline.Add(5 * time.Second))
	return c
}

// exchange sends one request and reads the echo back, recording latency.
func exchange(c net.Conn, req, resp []byte, st *clientStats) bool {
	t0 := time.Now()
	if _, err := c.Write(req); err != nil {
		st.fail("write", err)
		return false
	}
	if _, err := io.ReadFull(c, resp); err != nil {
		st.fail("read", err)
		return false
	}
	st.hist.record(time.Since(t0))
	st.reqs.Add(1)
	st.bytes.Add(uint64(len(req)))
	return true
}

func persistentWorker(cfg clientConfig, st *clientStats, deadline time.Time) {
	req, resp := make([]byte, cfg.msg), make([]byte, cfg.msg)
	fill(req)
	for time.Now().Before(deadline) {
		c := dial(cfg, st, deadline)
		if c == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for time.Now().Before(deadline) && exchange(c, req, resp, st) {
		}
		c.Close()
	}
}

func reconnectWorker(cfg clientConfig, st *clientStats, deadline time.Time) {
	req, resp := make([]byte, cfg.msg), make([]byte, cfg.msg)
	fill(req)
	for time.Now().Before(deadline) {
		c := dial(cfg, st, deadline)
		if c == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		exchange(c, req, resp, st)
		c.Close()
	}
}

func bulkWorker(cfg clientConfig, st *clientStats, deadline time.Time) {
	c := dial(cfg, st, deadline)
	if c == nil {
		return
	}
	defer c.Close()
	block := make([]byte, 64<<10)
	fill(block)
	if cfg.sinkOnly {
		for time.Now().Before(deadline) {
			n, err := c.Write(block)
			st.bytes.Add(uint64(n))
			if err != nil {
				if time.Now().Before(deadline) {
					st.fail("write", err)
				}
				return
			}
		}
		return
	}
	if cfg.recvOnly {
		buf := make([]byte, 64<<10)
		for time.Now().Before(deadline) {
			n, err := c.Read(buf)
			st.bytes.Add(uint64(n))
			if err != nil {
				if time.Now().Before(deadline) {
					st.fail("read", err)
				}
				return
			}
		}
		return
	}
	go func() {
		for time.Now().Before(deadline) {
			if _, err := c.Write(block); err != nil {
				if time.Now().Before(deadline) {
					st.fail("write", err)
				}
				return
			}
		}
	}()
	buf := make([]byte, 64<<10)
	for time.Now().Before(deadline) {
		n, err := c.Read(buf)
		st.bytes.Add(uint64(n))
		if err != nil {
			if time.Now().Before(deadline) {
				st.fail("read", err)
			}
			return
		}
	}
}

func fill(b []byte) {
	for i := range b {
		b[i] = byte('a' + i%26)
	}
}

// latencyHist is a two-tier histogram: 10 µs buckets up to 200 ms, then 10 ms
// buckets up to 30 s, then one overflow bucket. The coarse tier is for
// transfers whose time is set by their size (a 100 MB object at 10 Gbit/s is
// 80 ms and up), which the fine tier alone would report as "the maximum".
// Lock-free to record, so many workers can share it.
type latencyHist struct {
	buckets [20001]atomic.Uint64
	coarse  [3001]atomic.Uint64
}

const (
	histFineMax   = 20000 * histBucket // 200 ms
	histCoarse    = 10 * time.Millisecond
	histCoarseMax = 3000 * histCoarse // 30 s
)

const histBucket = 10 * time.Microsecond

func (h *latencyHist) record(d time.Duration) {
	if d < histFineMax {
		h.buckets[int(d/histBucket)].Add(1)
		return
	}
	i := int(d / histCoarse)
	if i >= len(h.coarse) {
		i = len(h.coarse) - 1
	}
	h.coarse[i].Add(1)
}

func (h *latencyHist) percentiles(ps ...float64) []time.Duration {
	var total uint64
	for i := range h.buckets {
		total += h.buckets[i].Load()
	}
	for i := range h.coarse {
		total += h.coarse[i].Load()
	}
	sort.Float64s(ps)
	out := make([]time.Duration, 0, len(ps))
	var cum uint64
	pi := 0
	for i := range h.buckets {
		cum += h.buckets[i].Load()
		for pi < len(ps) && float64(cum) >= ps[pi]*float64(total) {
			out = append(out, time.Duration(i)*histBucket)
			pi++
		}
	}
	for i := range h.coarse {
		cum += h.coarse[i].Load()
		for pi < len(ps) && float64(cum) >= ps[pi]*float64(total) {
			out = append(out, time.Duration(i)*histCoarse)
			pi++
		}
	}
	for len(out) < len(ps) {
		out = append(out, histCoarseMax)
	}
	return out
}
