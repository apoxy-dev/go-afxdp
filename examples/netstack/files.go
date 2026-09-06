//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
)

// A file-fetch workload: many clients dialling, fetching objects of mixed
// sizes, and hanging up — the shape of a CDN's traffic rather than a single
// bulk stream. The protocol is as dumb as it can be while still being a
// server: the client sends "GET /<name>\n", the server writes that object's
// bytes, and the connection stays open for the next request until the client
// closes it. No headers to parse, so the measurement is the stack.
//
// The client closes first, so TIME-WAIT accumulates on the client (where
// tcp_tw_reuse can recycle it) rather than on the server under test.

// sizeClass is one object in the mix.
type sizeClass struct {
	name   string
	bytes  int
	weight int
}

// maxRequestLine bounds a request line on every path: the run-to-completion
// server closes a connection whose request passes it (rtc_files.go), and so
// do the goroutine-per-connection servers here.
const maxRequestLine = 4096

// parseMix reads "64k:50,1m:30,5m:15,100m:5" — name:weight, the name being
// the object's size.
func parseMix(s string) ([]sizeClass, error) {
	var out []sizeClass
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, wt, ok := strings.Cut(part, ":")
		w := 1
		if ok {
			n, err := strconv.Atoi(wt)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%q: weight must be a positive number", part)
			}
			w = n
		}
		b, err := parseSize(name)
		if err != nil {
			return nil, err
		}
		out = append(out, sizeClass{name: name, bytes: b, weight: w})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty mix")
	}
	return out, nil
}

func parseSize(name string) (int, error) {
	mult := 1
	num := name
	switch {
	case strings.HasSuffix(name, "k"), strings.HasSuffix(name, "K"):
		mult, num = 1<<10, name[:len(name)-1]
	case strings.HasSuffix(name, "m"), strings.HasSuffix(name, "M"):
		mult, num = 1<<20, name[:len(name)-1]
	case strings.HasSuffix(name, "g"), strings.HasSuffix(name, "G"):
		mult, num = 1<<30, name[:len(name)-1]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: not a size like 64k, 1m, 100m", name)
	}
	return n * mult, nil
}

// fileSet is what the server serves and the client asks for.
type fileSet struct {
	classes []sizeClass
	byName  map[string]int // name -> bytes
	byIndex map[string]int // name -> class index
	pick    []int          // weighted index table
	body    []byte         // the largest object; every object is a prefix of it

	// The zero-copy layout (-mode files-rtc): each object is a run of cache
	// frames, replicated so that connections serving the same hot object do
	// not all fight for the same frames' header room.
	cache    *xdplink.TxCache
	plen     int   // payload bytes per frame
	replicas int   // copies of every object
	first    []int // [class*replicas + replica] -> first frame
	frames   []int // per class: frames per copy

	// The kernel's zero-copy path (-sendfile): one tmpfs file per object.
	fds []int
}

func newFileSet(mix string) (*fileSet, error) {
	classes, err := parseMix(mix)
	if err != nil {
		return nil, err
	}
	f := &fileSet{classes: classes, byName: map[string]int{}, byIndex: map[string]int{}}
	largest := 0
	for i, c := range classes {
		f.byName[c.name] = c.bytes
		f.byIndex[c.name] = i
		if c.bytes > largest {
			largest = c.bytes
		}
		for w := 0; w < c.weight; w++ {
			f.pick = append(f.pick, i)
		}
	}
	f.body = make([]byte, largest)
	fill(f.body)
	return f, nil
}

func (f *fileSet) random(r *rand.Rand) sizeClass { return f.classes[f.pick[r.Intn(len(f.pick))]] }

// serveFiles is the server side of one connection.
func (srv *server) serveFiles(c net.Conn, f *fileSet) {
	defer c.Close()
	srv.accepted.Add(1)
	defer srv.closed.Add(1)
	br := bufio.NewReaderSize(c, maxRequestLine)
	for {
		// ReadSlice fails with ErrBufferFull past the buffer, which is the
		// bound: a peer that never sends a newline gets closed, not a
		// growing string (ReadString would keep appending).
		raw, err := br.ReadSlice('\n')
		if err != nil {
			return
		}
		line := string(raw)
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "GET /"))
		n, ok := f.byName[name]
		if !ok {
			return
		}
		srv.bytesIn.Add(uint64(len(line)))
		if _, err := c.Write(f.body[:n]); err != nil {
			return
		}
		srv.bytesOut.Add(uint64(n))
	}
}

// ---- the load generator ----

type filesConfig struct {
	target    string
	workers   int
	perConn   int // requests before the client closes (0 = keep open for the run)
	duration  time.Duration
	mix       string
	processes int
	// slow, when set, is the bytes a second one connection reads: the
	// client paces itself so connections live for seconds and the server
	// holds their data. What that measures is the cost of a connection,
	// not throughput.
	slow int
	// ramp spreads the workers' first dials over this long. Started in the
	// same instant, 2000 workers hand the server one burst of 2000
	// requests, and that burst alone was the first byte's p99 for the
	// whole run (130 ms against 10 ms on the wire once running). Clients
	// of a real server arrive spread out.
	ramp time.Duration
}

type filesStats struct {
	conns, reqs, bytes, errs atomic.Uint64
	perClass                 []classStats
	el                       errLog
	elMu                     sync.Mutex
}

type classStats struct {
	name  string
	count atomic.Uint64
	bytes atomic.Uint64
	hist  latencyHist // whole-object time
	ttfb  latencyHist // time to first byte
}

func (st *filesStats) fail(what string, err error) {
	st.errs.Add(1)
	st.elMu.Lock()
	st.el.printf("%s: %v", what, err)
	st.elMu.Unlock()
}

// runFilesClient drives the workload from this box with every core it has.
func runFilesClient(cfg filesConfig) {
	f, err := newFileSet(cfg.mix)
	if err != nil {
		log.Fatalf("-mix: %v", err)
	}
	if cfg.workers <= 0 {
		log.Fatalf("-workers must be positive")
	}
	st := newFilesStats(f)
	targets := expandTargets(cfg.target)
	log.Printf("files client: %s, %d workers, mix %s, %d req/conn, for %s",
		strings.Join(targets, ","), cfg.workers, cfg.mix, cfg.perConn, cfg.duration)

	deadline := time.Now().Add(cfg.duration)
	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			filesWorker(cfg, f, st, targets, deadline, rand.New(rand.NewSource(int64(seed)*2654435761+1)), seed)
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	filesReport(cfg, st, f, done)
}

// expandTargets turns "ip:8080-8087" or "ip:8080,ip:8081" into a list, so
// the client's ephemeral ports are spread over several four-tuple spaces.
func expandTargets(spec string) []string {
	var out []string
	for _, part := range strings.Split(spec, ",") {
		host, ports, err := net.SplitHostPort(part)
		if err != nil {
			out = append(out, part)
			continue
		}
		lo, hi, ok := strings.Cut(ports, "-")
		if !ok {
			out = append(out, part)
			continue
		}
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || b < a {
			out = append(out, part)
			continue
		}
		for p := a; p <= b; p++ {
			out = append(out, net.JoinHostPort(host, strconv.Itoa(p)))
		}
	}
	return out
}

func filesWorker(cfg filesConfig, f *fileSet, st *filesStats, targets []string, deadline time.Time, r *rand.Rand, seed int) {
	if cfg.ramp > 0 && cfg.workers > 0 {
		time.Sleep(cfg.ramp * time.Duration(seed) / time.Duration(cfg.workers))
	}
	buf := make([]byte, 256<<10)
	if cfg.slow > 0 && len(buf) > cfg.slow {
		// Read in pieces the pacing can space out; a whole buffer at a
		// slow rate would be one long sleep per read.
		buf = buf[:max(cfg.slow/10, 1500)]
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		c, err := dialer.Dial("tcp", targets[r.Intn(len(targets))])
		if err != nil {
			st.fail("dial", err)
			time.Sleep(time.Millisecond)
			continue
		}
		if t, ok := c.(*net.TCPConn); ok {
			t.SetNoDelay(true)
		}
		if cfg.slow > 0 {
			// A small receive buffer is half of being slow: without it the
			// kernel takes the whole object at line rate and only the
			// application lags behind.
			if t, ok := c.(*net.TCPConn); ok {
				_ = t.SetReadBuffer(64 << 10)
			}
		}
		st.conns.Add(1)
		n := cfg.perConn
		for i := 0; (n == 0 || i < n) && time.Now().Before(deadline); i++ {
			if !fetch(cfg, c, f, st, buf, r) {
				break
			}
		}
		c.Close()
	}
}

// fetch asks for one object and reads it, timing the first byte and the whole
// transfer. It reports false when the connection is done.
func fetch(cfg filesConfig, c net.Conn, f *fileSet, st *filesStats, buf []byte, r *rand.Rand) bool {
	idx := f.pick[r.Intn(len(f.pick))]
	class := f.classes[idx]
	cs := &st.perClass[idx]
	start := time.Now()
	if _, err := io.WriteString(c, "GET /"+class.name+"\n"); err != nil {
		st.fail("write", err)
		return false
	}
	left := class.bytes
	first := true
	for left > 0 {
		p := buf
		if left < len(p) {
			p = p[:left]
		}
		k, err := c.Read(p)
		if k > 0 {
			if first {
				cs.ttfb.record(time.Since(start))
				first = false
			}
			left -= k
			st.bytes.Add(uint64(k))
			cs.bytes.Add(uint64(k))
			if cfg.slow > 0 {
				// Sleep back to the pace: where this read should have
				// ended, less where we are.
				want := time.Duration(float64(class.bytes-left) / float64(cfg.slow) * float64(time.Second))
				if d := want - time.Since(start); d > 0 {
					time.Sleep(d)
				}
			}
		}
		if err != nil {
			st.fail("read", err)
			return false
		}
	}
	cs.hist.record(time.Since(start))
	cs.count.Add(1)
	st.reqs.Add(1)
	return true
}

func filesReport(cfg filesConfig, st *filesStats, f *fileSet, done <-chan struct{}) {
	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last struct{ conns, reqs, bytes uint64 }
	// The summary is printed at the deadline, from the counters as they
	// stand then: the objects still in flight (a 100 MB one can take
	// twenty seconds more at its fair share) are drained afterwards and
	// do not count, and nobody has to wait for them to read the result.
	deadline := time.After(cfg.duration)
	summary := func() {
		el := time.Since(start).Seconds()
		c, r, b, e := st.conns.Load(), st.reqs.Load(), st.bytes.Load(), st.errs.Load()
		log.Printf("total over %.1fs: %.0f conn/s  %.0f obj/s  %s  (%d conns, %d objects, %d errors)",
			el, float64(c)/el, float64(r)/el, gbps(uint64(float64(b)/el)), c, r, e)
		for i := range st.perClass {
			cs := &st.perClass[i]
			n := cs.count.Load()
			if n == 0 {
				continue
			}
			p := cs.hist.percentiles(0.5, 0.99)
			t := cs.ttfb.percentiles(0.5, 0.99)
			log.Printf("  %-5s %8d objects  %6.0f/s  %s  first byte p50 %s p99 %s  complete p50 %s p99 %s",
				cs.name, n, float64(n)/el, gbps(uint64(float64(cs.bytes.Load())/el)), t[0], t[1], p[0], p[1])
		}
	}
	printed := false
	for {
		select {
		case <-tick.C:
			c, r, b := st.conns.Load(), st.reqs.Load(), st.bytes.Load()
			log.Printf("conns %d/s  objects %d/s  %s  errs %d",
				c-last.conns, r-last.reqs, gbps(b-last.bytes), st.errs.Load())
			last.conns, last.reqs, last.bytes = c, r, b
		case <-deadline:
			if !printed {
				summary()
				printed = true
			}
			deadline = nil
		case <-done:
			if !printed {
				summary()
			}
			return
		}
	}
}

// tuneClientPorts widens the ephemeral port range and turns on TIME-WAIT
// reuse, which a client dialling tens of thousands of times a second needs.
// It reports what it changed so the caller can put it back.
func tuneClientPorts() (restore func()) {
	type knob struct{ path, want string }
	knobs := []knob{
		{"/proc/sys/net/ipv4/ip_local_port_range", "1024\t65535"},
		{"/proc/sys/net/ipv4/tcp_tw_reuse", "1"},
		{"/proc/sys/net/ipv4/tcp_fin_timeout", "5"},
	}
	var undo []func()
	for _, k := range knobs {
		old, err := os.ReadFile(k.path)
		if err != nil {
			continue
		}
		if err := os.WriteFile(k.path, []byte(k.want), 0); err != nil {
			log.Printf("client tuning: %s: %v (continuing)", k.path, err)
			continue
		}
		p, v := k.path, string(old)
		undo = append(undo, func() { _ = os.WriteFile(p, []byte(v), 0) })
	}
	return func() {
		for _, f := range undo {
			f()
		}
	}
}

// sortedNames is for stable reporting.
func (f *fileSet) sortedNames() []string {
	names := make([]string, 0, len(f.classes))
	for _, c := range f.classes {
		names = append(names, c.name)
	}
	sort.Strings(names)
	return names
}

// cacheFramesNeeded is the cache size in bytes for the mix with r replicas
// of every object, given the frame size and payload per frame.
func (f *fileSet) cacheFramesNeeded(frameSize, plen, replicas int) int {
	total := 0
	for _, c := range f.classes {
		total += (c.bytes + plen - 1) / plen
	}
	return total * replicas * frameSize
}

// layoutInCache places every object (replicas copies each) in the cache and
// fills the frames with the pattern. plen is the payload per frame.
func (f *fileSet) layoutInCache(c *xdplink.TxCache, plen, replicas int) error {
	f.cache, f.plen, f.replicas = c, plen, replicas
	f.frames = make([]int, len(f.classes))
	f.first = make([]int, len(f.classes)*replicas)
	next := 0
	for i, cl := range f.classes {
		n := (cl.bytes + plen - 1) / plen
		f.frames[i] = n
		for r := 0; r < replicas; r++ {
			if next+n > c.Frames() {
				return fmt.Errorf("cache too small: %d frames, need %d", c.Frames(), f.cacheFramesNeeded(1, plen, replicas))
			}
			f.first[i*replicas+r] = next
			left := cl.bytes
			for k := 0; k < n; k++ {
				p := c.Payload(next + k)[:min(plen, left)]
				copy(p, f.body[cl.bytes-left:])
				left -= len(p)
			}
			next += n
		}
	}
	return nil
}

// object returns where copy r of class i lives: its first frame and count.
func (f *fileSet) object(i, r int) (first, frames int) {
	return f.first[i*f.replicas+r%f.replicas], f.frames[i]
}

// writeTmpfsFiles writes every object to /dev/shm, for the kernel's sendfile
// path, and keeps the descriptors.
func (f *fileSet) writeTmpfsFiles() error {
	for _, c := range f.classes {
		name := "/dev/shm/netstack-obj-" + c.name
		if err := os.WriteFile(name, f.body[:c.bytes], 0o644); err != nil {
			return err
		}
		fd, err := syscall.Open(name, syscall.O_RDONLY, 0)
		if err != nil {
			return err
		}
		f.fds = append(f.fds, fd)
	}
	return nil
}

// serveFilesSendfile is serveFiles with the kernel's zero-copy transmit: the
// object goes from the page cache to the socket without passing through
// user space, which is what a content server on the kernel actually does.
func (srv *server) serveFilesSendfile(c net.Conn, f *fileSet) {
	defer c.Close()
	srv.accepted.Add(1)
	defer srv.closed.Add(1)
	tc, ok := c.(*net.TCPConn)
	if !ok {
		srv.serveFiles(c, f)
		return
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return
	}
	br := bufio.NewReaderSize(c, maxRequestLine)
	for {
		// ReadSlice fails with ErrBufferFull past the buffer, which is the
		// bound: a peer that never sends a newline gets closed, not a
		// growing string (ReadString would keep appending).
		raw, err := br.ReadSlice('\n')
		if err != nil {
			return
		}
		line := string(raw)
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "GET /"))
		i, ok := f.byIndex[name]
		if !ok {
			return
		}
		srv.bytesIn.Add(uint64(len(line)))
		n := f.classes[i].bytes
		off := int64(0)
		left := n
		var serr error
		for left > 0 && serr == nil {
			werr := rc.Write(func(fd uintptr) bool {
				k, e := syscall.Sendfile(int(fd), f.fds[i], &off, left)
				if k > 0 {
					left -= k
				}
				if e == syscall.EAGAIN {
					return false // wait for writability, then come back
				}
				serr = e
				return true
			})
			if werr != nil {
				return
			}
		}
		if serr != nil {
			return
		}
		srv.bytesOut.Add(uint64(n))
	}
}

// newFilesStats is one counter set per size class, named for the report.
func newFilesStats(f *fileSet) *filesStats {
	st := &filesStats{perClass: make([]classStats, len(f.classes))}
	for i, c := range f.classes {
		st.perClass[i].name = c.name
	}
	return st
}
