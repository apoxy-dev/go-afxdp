//go:build linux

// netstack is a TCP server that terminates connections in userspace: gVisor's
// TCP/IP stack (gvisor.dev/gvisor/pkg/tcpip) fed straight from AF_XDP rings
// through the xdplink package next to this file. The same server also runs on
// ordinary kernel sockets, so the two stacks can be compared with identical
// application code: connections per second, requests per second, bytes per
// second, and the CPU each costs.
//
//	cd examples/netstack && go build .
//	sudo ./netstack -iface eth0 -ip 192.0.2.10/24 -port 8080 -mode echo
//	sudo ./netstack -iface eth0 -ip 192.0.2.10/24 -port 8080 -stack kernel   # the comparison
//
// Only TCP to -port is taken from the kernel; ARP, SSH and everything else
// keep flowing normally, and the kernel keeps answering ARP for -ip, which it
// must own too (on the interface, or on its VLAN sub-interface when -vlan is
// set). Test it from another host with nc, curl (-mode http), or the built-in
// load generator:
//
//	./netstack -client 192.0.2.10:8080 -conns 64 -msg 64 -duration 10s
//
// On a NIC that carries only tagged traffic, pass -vlan and turn off VLAN
// offload first, or XDP never sees the tag: ethtool -K eth0 rxvlan off txvlan off.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atoonk/go-afxdp"
	"github.com/atoonk/go-afxdp/examples/netstack/xdplink"
	"github.com/cilium/ebpf/rlimit"
	"gvisor.dev/gvisor/pkg/refs"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/qdisc/fifo"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func main() {
	iface := flag.String("iface", "eth0", "interface to bind")
	vlan := flag.Int("vlan", 0, "802.1Q tag on every frame (0 = untagged)")
	ipFlag := flag.String("ip", "", "address to serve on, as ip/prefix (default: the interface's, or its -vlan sub-interface's, first IPv4)")
	port := flag.Int("port", 8080, "TCP port to serve")
	ports := flag.Int("ports", 1, "serve this many consecutive ports from -port: one four-tuple space per port, for a client that opens tens of thousands of connections from one address")
	mtu := flag.Int("mtu", 1500, "IP MTU the stack assumes (the VLAN's, not the trunk's)")
	queues := flag.Int("queues", 0, "rx queues to bind (0 = all; RSS spreads connections across them)")
	stackKind := flag.String("stack", "netstack", "netstack (gVisor over AF_XDP) or kernel (plain sockets)")
	peerMAC := flag.String("peer-mac", "", "with -client-stack netstack: the server's MAC, so nothing is asked of the kernel's ARP table (default: look it up there)")
	clientStackKind := flag.String("client-stack", "kernel", "with -client: kernel (plain sockets) or netstack (the same stack, as a load generator; needs -iface/-vlan/-ip)")
	mode := flag.String("mode", "echo", "echo (write back what is read; reader and writer decoupled), echo-rtc (netstack only: run-to-completion on the receive goroutines, no goroutine per connection), echo-simple (one goroutine: read then write), http (a fixed 200 for wrk/ab), sink (drain), source (stream), files (dial-and-fetch objects of mixed sizes, the -mix flag), source-zc (netstack+fork only: stream the -cache content without copying it), sink-rtc (netstack only: drain on the receive goroutines)")
	filesMode := flag.Bool("files", false, "with -client: the file-fetch workload (dial, fetch objects of mixed sizes, close) instead of request/response or bulk")
	workers := flag.Int("workers", 1000, "with -client -files: concurrent client goroutines")
	rtcBudgetTimeFlag := flag.Duration("rtc-budget", 200*time.Microsecond, "how long one run-to-completion pass services ready connections before it goes back to receiving (0 = until the ready list is empty)")
	rtcBudgetConnsFlag := flag.Int("rtc-budget-conns", 0, "also bound that pass to this many connections (0 = no count bound)")
	ramp := flag.Duration("ramp", time.Second, "with -client -files: spread the workers' first dials over this long, so the run does not open with one burst of -workers requests")
	slow := flag.String("slow", "", "with -client -files: pace each connection to this many bytes a second (e.g. 50k), so connections live for seconds — the measurement is per-connection cost, not throughput")
	perConn := flag.Int("per-conn", 4, "with -client -files: objects fetched per connection before the client closes it (0 = keep it open for the run)")
	mix := flag.String("mix", "64k:50,1m:30,5m:15,100m:5", "with -files: the object mix as size:weight, drawn per request")
	replicas := flag.Int("replicas", 8, "with -mode files-rtc: copies of every object in the cache, so connections serving the same hot object do not all contend for its frames")
	sendfile := flag.Bool("sendfile", false, "with -stack kernel -mode files: serve objects from tmpfs with sendfile(2), the kernel's own zero-copy path")
	cacheSize := flag.Int("cache", 0, "bytes of content held in the shared UMEM for -mode source-zc (0 = none; the whole UMEM is then per socket as usual)")
	txMode := flag.String("tx", "direct", "netstack transmit path: direct (the writer transmits itself, per-queue lock; lowest latency) or fifo (a goroutine per queue batches transmits; fewer syscalls under bulk)")
	useGRO := flag.Bool("gro", true, "netstack: coalesce bursts of received segments before they enter the stack")
	useGSO := flag.Bool("gso", true, "netstack: segmentation offload — TCP hands the endpoint 64 KiB packets and it splits them")
	needWakeup := flag.Bool("needwakeup", true, "bind with XDP_USE_NEED_WAKEUP so a zero-copy driver needs no syscall per transmit batch")
	busyPoll := flag.Int("busypoll", 0, "kernel busy-poll microseconds for the AF_XDP sockets (0 = off)")
	maxProcs := flag.Int("gomaxprocs", 0, "GOMAXPROCS for the server (0 = Go default)")
	stacksMode := flag.String("stacks", "per-queue", "netstack: per-queue (one independent stack per NIC queue, shared-nothing) or single (one stack over all queues)")
	deferKicks := flag.Bool("deferkicks", true, "queue transmits without a kick and let each receive goroutine kick its socket once per batch (one syscall per batch, not per packet)")
	sndBufFlag := flag.Int("sndbuf", 8<<20, "netstack TCP send buffer per connection, bytes (8 MiB keeps a 10G bulk stream full; 1 MiB serves 100k connections in 100 GiB)")
	rcvBufFlag := flag.Int("rcvbuf", 8<<20, "netstack TCP receive buffer per connection, bytes")
	txBacklog := flag.String("txbacklog", "64,32", "high,low water marks of a queue's transmit backlog, in packets: at high the stack is refused and pauses, below low it is told to resume (fork only; 0 = drop beyond a cap, as upstream would)")
	txInflight := flag.Int("txinflight", 256, "frames a queue may have in the transmit ring awaiting completion; the rest wait in the endpoint (0 = the ring's depth). Keep it under the NIC's hardware ring or its driver spins a core per queue waiting for room")
	groHold := flag.Duration("grohold", 200*time.Microsecond, "let a GRO merge span receive batches for up to this long (0 = flush every batch); a bulk receiver then ACKs per merged packet instead of per batch, which is the sender's cost")
	rxCsum := flag.String("rxcsum", "verify", "received TCP/IPv4 checksums: verify (software, the honest default) or trust (skip it: the NIC checked, XDP just does not say)")
	txCsum := flag.String("txcsum", "auto", "TCP checksum on transmit: auto (NIC offload if the driver reports it, else software), on, off")
	cpus := flag.String("cpus", "", "pin queue i's receive goroutine to the i-th CPU of this comma-separated list (with -affinity=false; put each queue's interrupt on a different core)")
	affinity := flag.Bool("affinity", true, "let go-afxdp place each queue's worker and steer its interrupt (off when the operator confines the process with taskset and steers interrupts themselves)")
	autoTune := flag.Bool("autotune", true, "let go-afxdp defer NAPI interrupts (batches packets; adds up to 20µs latency per packet at low rates)")
	recovery := flag.Int("recovery", -1, "netstack TCPRecovery bitmask (-1 = gVisor default RACK; 0 = classic/SACK only)")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile here until stopped")
	mutexprofile := flag.String("mutexprofile", "", "write a mutex contention profile here at stop (sampling 1 in 10)")
	blockprofile := flag.String("blockprofile", "", "write a blocking profile here at stop (events over 10µs)")
	memprofile := flag.String("memprofile", "", "write an allocation profile here at stop (alloc_space: what was allocated over the run)")
	traceFile := flag.String("trace", "", "write a runtime trace here for the -trace-secs after startup")
	traceSecs := flag.Int("trace-secs", 2, "length of the -trace window, seconds")
	frames := flag.Int("frames", 16384, "UMEM frames per queue (rx + tx)")
	txFrames := flag.Int("txframes", 8192, "of -frames, how many are reserved for transmit (a GSO batch can need 2200)")
	ringSize := flag.Int("ring", 4096, "descriptors per ring (fill, rx, tx, completion); power of two")

	clientAddr := flag.String("client", "", "run as a load generator against this ip:port instead of serving")
	conns := flag.Int("conns", 64, "client: concurrent connections")
	msg := flag.Int("msg", 64, "client: bytes per request")
	duration := flag.Duration("duration", 10*time.Second, "client: how long to run")
	bulk := flag.Bool("bulk", false, "client: stream 64 KiB writes instead of request/response")
	reconnect := flag.Bool("reconnect", false, "client: one request per connection, reconnecting every time")
	sinkOnly := flag.Bool("sink", false, "client: with -bulk, only send (server -mode sink); one-way test")
	recvOnly := flag.Bool("recv", false, "client: with -bulk, only receive (server -mode source); one-way test")
	flag.Parse()

	applyForkFlags()
	rxFrames, txFramesN, ringN = *frames, *txFrames, *ringSize
	fleetTune = fleetTuning{needWakeup: *needWakeup, busyPollUsecs: *busyPoll, noAutoTune: !*autoTune}
	deferKicksFlag = *deferKicks
	txCsumMode = *txCsum
	fleetTune.noAffinity = !*affinity
	for _, f := range strings.Split(*cpus, ",") {
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			log.Fatalf("-cpus %q: %v", *cpus, err)
		}
		pinCPUs = append(pinCPUs, n)
	}
	rxCsumTrust = *rxCsum == "trust"
	sndBuf, rcvBuf = *sndBufFlag, *rcvBufFlag
	groHoldFlag = *groHold
	txInflightFlag = *txInflight
	if *ports < 1 || *port+*ports-1 > 65535 {
		log.Fatalf("-ports %d: want 1..%d", *ports, 65536-*port)
	}
	portCount = *ports
	rtcBudgetTime, rtcBudget = *rtcBudgetTimeFlag, *rtcBudgetConnsFlag
	if forkLinkBackpressure() {
		if _, err := fmt.Sscanf(*txBacklog, "%d,%d", &txBacklogHigh, &txBacklogLow); err != nil {
			if _, err2 := fmt.Sscanf(*txBacklog, "%d", &txBacklogHigh); err2 != nil {
				log.Fatalf("-txbacklog %q: want high or high,low", *txBacklog)
			}
			txBacklogLow = txBacklogHigh / 2
		}
		switch {
		case txBacklogHigh < 0 || txBacklogLow < 0:
			log.Fatalf("-txbacklog %q: marks cannot be negative", *txBacklog)
		case txBacklogHigh > 0 && (txBacklogLow < 1 || txBacklogLow > txBacklogHigh):
			log.Fatalf("-txbacklog %q: low must be between 1 and high", *txBacklog)
		}
		if txBacklogHigh > 0 {
			log.Printf("transmit backlog: refuse at %d packets, resume below %d (-txbacklog)", txBacklogHigh, txBacklogLow)
		} else {
			log.Printf("transmit backlog: unbounded, drops beyond the cap (-txbacklog 0)")
		}
	}
	if *maxProcs > 0 {
		runtime.GOMAXPROCS(*maxProcs)
	}
	// gVisor registers every reference-counted object (each packet buffer,
	// each chunk) in a leak-check map unless told not to: a locked map
	// insert and delete per packet, visible in the 100G receive profile.
	refs.SetLeakMode(refs.NoLeakChecking)
	if *mutexprofile != "" {
		runtime.SetMutexProfileFraction(10)
	}
	if *blockprofile != "" {
		runtime.SetBlockProfileRate(10000)
	}
	writeProfiles := func() {
		if *memprofile != "" {
			if f, err := os.Create(*memprofile); err == nil {
				_ = pprof.Lookup("allocs").WriteTo(f, 0)
				f.Close()
			}
		}
		if *mutexprofile != "" {
			if f, err := os.Create(*mutexprofile); err == nil {
				_ = pprof.Lookup("mutex").WriteTo(f, 0)
				f.Close()
			}
		}
		if *blockprofile != "" {
			if f, err := os.Create(*blockprofile); err == nil {
				_ = pprof.Lookup("block").WriteTo(f, 0)
				f.Close()
			}
		}
	}
	defer writeProfiles()
	if *traceFile != "" {
		go func() {
			time.Sleep(8 * time.Second) // past startup and the client's ramp
			f, err := os.Create(*traceFile)
			if err != nil {
				return
			}
			if err := trace.Start(f); err == nil {
				time.Sleep(time.Duration(*traceSecs) * time.Second)
				trace.Stop()
			}
			f.Close()
		}()
	}
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatalf("-cpuprofile: %v", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatalf("-cpuprofile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	if *clientAddr != "" && *filesMode {
		restore := tuneClientPorts()
		defer restore()
		slowRate := 0
		if *slow != "" {
			n, err := parseSize(*slow)
			if err != nil {
				log.Fatalf("-slow %q: %v", *slow, err)
			}
			slowRate = n
		}
		fcfg := filesConfig{target: *clientAddr, workers: *workers, perConn: *perConn,
			duration: *duration, mix: *mix, slow: slowRate, ramp: *ramp}
		if *clientStackKind == "netstack" {
			prefix, err := resolveIP(*iface, *vlan, *ipFlag)
			if err != nil {
				log.Fatalf("%v", err)
			}
			tm := xdplink.TxDirect
			if *txMode == "fifo" {
				tm = xdplink.TxFIFO
			}
			peerMACFlag = *peerMAC
			runNetstackFilesClient(fcfg, *iface, uint16(*vlan), prefix, uint32(*mtu), tm, *useGRO, *useGSO)
			return
		}
		runFilesClient(fcfg)
		return
	}
	if *clientAddr != "" {
		cfg := clientConfig{target: *clientAddr, conns: *conns, msg: *msg,
			duration: *duration, bulk: *bulk, reconnect: *reconnect, sinkOnly: *sinkOnly, recvOnly: *recvOnly}
		if *clientStackKind == "netstack" {
			prefix, err := resolveIP(*iface, *vlan, *ipFlag)
			if err != nil {
				log.Fatalf("%v", err)
			}
			tm := xdplink.TxDirect
			if *txMode == "fifo" {
				tm = xdplink.TxFIFO
			}
			peerMACFlag = *peerMAC
			runNetstackClient(cfg, *iface, uint16(*vlan), prefix, uint32(*mtu), tm, *useGRO, *useGSO)
			return
		}
		runClient(cfg)
		return
	}

	if *port <= 0 || *port > 65535 {
		log.Fatalf("-port %d out of range", *port)
	}
	if *vlan < 0 || *vlan > 4094 {
		log.Fatalf("-vlan %d out of range", *vlan)
	}
	prefix, err := resolveIP(*iface, *vlan, *ipFlag)
	if err != nil {
		log.Fatalf("%v", err)
	}
	addr := netip.AddrPortFrom(prefix.Addr(), uint16(*port))

	var srv server
	if *mode == "files" {
		fs, err := newFileSet(*mix)
		if err != nil {
			log.Fatalf("-mix: %v", err)
		}
		srv.files = fs
		log.Printf("serving objects: %s", *mix)
	}
	var kernelLns []net.Listener
	var ns *netstackRuntime
	var newRTC func() *rtcServer
	if *mode == "files-rtc" {
		if *stackKind != "netstack" {
			log.Fatalf("-mode files-rtc runs on netstack only")
		}
		fs, err := newFileSet(*mix)
		if err != nil {
			log.Fatalf("-mix: %v", err)
		}
		srv.files = fs
		if *cacheSize == 0 {
			// Frames of 2 KiB holding one segment's payload each, every
			// object replicated, plus a little slack for rounding.
			*cacheSize = fs.cacheFramesNeeded(2048, int(*mtu)-80, *replicas) + 1<<20
		}
		log.Printf("serving objects from the cache: %s, %d replicas, %d MiB", *mix, *replicas, *cacheSize>>20)
	}
	if *mode == "echo-rtc" || *mode == "source-zc" || *mode == "sink-rtc" || *mode == "files-rtc" {
		if *stackKind != "netstack" {
			log.Fatalf("-mode %s runs on netstack only", *mode)
		}
		if *mode == "source-zc" && *cacheSize <= 0 {
			log.Fatalf("-mode source-zc needs -cache <bytes>")
		}
		newRTC = func() *rtcServer {
			r := newRTCServer(&srv)
			if rtcFiles {
				r.files = srv.files
			}
			return r
		}
	}
	cacheBytes = *cacheSize
	rtcSource = *mode == "source-zc"
	rtcSink = *mode == "sink-rtc"
	rtcFiles = *mode == "files-rtc"
	if *mode == "files" && *sendfile {
		if *stackKind != "kernel" {
			log.Fatalf("-sendfile is the kernel's zero-copy path; use -mode files-rtc for netstack's")
		}
		if err := srv.files.writeTmpfsFiles(); err != nil {
			log.Fatalf("-sendfile: %v", err)
		}
		srv.sendfile = true
		log.Printf("serving objects with sendfile from /dev/shm")
	}
	var perQueue bool
	switch *stacksMode {
	case "per-queue":
		perQueue = true
	case "single":
	default:
		log.Fatalf("-stacks %q: want per-queue or single", *stacksMode)
	}
	switch *stackKind {
	case "kernel":
		for _, p := range portList(uint16(*port)) {
			l, err := net.Listen("tcp", netip.AddrPortFrom(prefix.Addr(), p).String())
			if err != nil {
				log.Fatalf("listen: %v", err)
			}
			kernelLns = append(kernelLns, l)
		}
		log.Printf("kernel stack: listening on %s (%d ports)", addr, len(kernelLns))
	case "netstack":
		var tm xdplink.TxMode
		switch *txMode {
		case "fifo":
			tm = xdplink.TxFIFO
		case "direct":
			tm = xdplink.TxDirect
		default:
			log.Fatalf("-tx %q: want fifo or direct", *txMode)
		}
		netstackRecovery = *recovery
		ns, err = startNetstack(*iface, uint16(*vlan), prefix, uint16(*port), uint32(*mtu), tm, *useGRO, *useGSO,
			perQueue, newRTC, afxdp.WithQueues(*queues))
		if err != nil {
			log.Fatalf("%v", err)
		}
		if rtcFiles {
			if ns.cache == nil {
				log.Fatalf("-mode files-rtc: no cache (is -cache 0?)")
			}
			if err := srv.files.layoutInCache(ns.cache, cachePayload(ns.cache), *replicas); err != nil {
				log.Fatalf("-mode files-rtc: %v", err)
			}
		}
		log.Printf("netstack: listening on %s via %s (%d stacks over %d queues, tx %s, gso %v, gro %v, need-wakeup %v, mode %s)",
			addr, *iface, len(ns.units), ns.fleet.NumQueues(), *txMode, *useGSO, *useGRO, *needWakeup, *mode)
	default:
		log.Fatalf("-stack %q: want netstack or kernel", *stackKind)
	}

	switch {
	case ns == nil:
		for _, l := range kernelLns {
			go srv.serve(l, *mode)
		}
	case newRTC == nil:
		for _, u := range ns.units {
			for _, l := range u.lns {
				go srv.serve(l, *mode)
			}
		}
	}
	go report(&srv, ns)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("stopping")
	srv.stopping.Store(true)
	if ns != nil {
		ns.stop()
	} else {
		for _, l := range kernelLns {
			l.Close()
		}
	}
	pprof.StopCPUProfile()
}

// resolveIP returns the address to serve on: -ip as given, or the first IPv4
// on the interface (its VLAN sub-interface when -vlan is set). Either way the
// kernel must own the address, because it is the kernel that answers ARP for
// it — see the xdplink package doc.
func resolveIP(iface string, vlan int, ipFlag string) (netip.Prefix, error) {
	if ipFlag != "" {
		p, err := netip.ParsePrefix(ipFlag)
		if err != nil || !p.Addr().Is4() {
			return netip.Prefix{}, fmt.Errorf("-ip %q: want an IPv4 address with a prefix, like 192.0.2.10/24", ipFlag)
		}
		return p, nil
	}
	names := []string{iface}
	if vlan != 0 {
		// Both common namings for a sub-interface: eth0.2131 and vlan.2131.
		names = []string{fmt.Sprintf("%s.%d", iface, vlan), fmt.Sprintf("vlan.%d", vlan)}
	}
	for _, name := range names {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil {
					bits, _ := ipn.Mask.Size()
					return netip.PrefixFrom(netip.AddrFrom4([4]byte(ip4)), bits), nil
				}
			}
		}
	}
	return netip.Prefix{}, fmt.Errorf("no IPv4 address on %s; add one (the kernel answers ARP for it) or pass -ip",
		strings.Join(names, " or "))
}

// netstackRuntime is everything -stack netstack brings up, in the order it
// must be torn down.
type netstackRuntime struct {
	fleet *afxdp.Fleet
	cache *xdplink.TxCache // -cache content in the shared UMEM, or nil
	units []stackUnit
	addr  tcpip.Address
	port  uint16
	// unit 0, for the single-stack case and the tests
	ep *xdplink.Endpoint
	s  *stack.Stack
	ln net.Listener
}

// stackUnit is one independent netstack over some of the fleet's queues —
// with -stacks per-queue, exactly one queue: its own receive goroutine, its
// own transmit ring, its own demuxer and processor, sharing nothing with its
// neighbours but the XDP program and the address. RSS decides which unit a
// connection lands on and keeps it there.
type stackUnit struct {
	ep  *xdplink.Endpoint
	s   *stack.Stack
	ln  net.Listener   // the first of lns, for callers that want one
	lns []net.Listener // gonet listeners, one per port; none in run-to-completion mode
	rtc *rtcServer     // run-to-completion server; nil otherwise
}

// startNetstack opens the fleet, builds the endpoint and the stack, and
// returns a listener on addr. extra are further fleet options (tests force
// generic mode on a veth).
var (
	rxFrames, txFramesN, ringN = 16384, 8192, 4096
	netstackRecovery           = -1
	fleetTune                  fleetTuning
	deferKicksFlag             = true
	txCsumMode                 = "auto"
	clientStack                = false // startNetstack builds a load generator's stack, not a server's
	clientTimeWait             = 200 * time.Millisecond
	sndBuf, rcvBuf             = 8 << 20, 8 << 20
	groHoldFlag                = 200 * time.Microsecond
	txInflightFlag             = 256
	// portCount is -ports: a server binds that many consecutive ports from
	// the one given, so a client with tens of thousands of connections has
	// more than one four-tuple space to spend.
	portCount                   = 1
	txBacklogHigh, txBacklogLow = 0, 0  // -txbacklog, applied only when the fork can pause the sender
	cacheBytes                  = 0     // -cache: shared UMEM content for -mode source-zc
	rtcSource                   = false // -mode source-zc: the rtc servers stream the cache
	rtcSink                     = false // -mode sink-rtc: the rtc servers read and discard
	rtcFiles                    = false // -mode files-rtc: the rtc servers serve the cache
	cacheMTU                    = 1500  // the -mtu the cache was sized for
	peerMACFlag                 = ""    // -peer-mac
	rxCsumTrust                 = false // -rxcsum trust
	pinCPUs                     []int   // -cpus
)

// fleetTuning carries the datapath knobs from flags into startNetstack.
type fleetTuning struct {
	needWakeup    bool
	busyPollUsecs int
	noAutoTune    bool
	noAffinity    bool
}

// portList is the ports a server binds: -ports of them, from port up.
func portList(port uint16) []uint16 {
	n := portCount
	if n < 1 {
		n = 1
	}
	ports := make([]uint16, n)
	for i := range ports {
		ports[i] = port + uint16(i)
	}
	return ports
}

func startNetstack(iface string, vlan uint16, prefix netip.Prefix, port uint16, mtu uint32,
	tm xdplink.TxMode, useGRO, useGSO, perQueue bool, newRTC func() *rtcServer, extra ...afxdp.Option) (*netstackRuntime, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %v", iface, err)
	}
	// The UMEMs are locked memory: 16384 frames x 2 KiB per queue. Running as
	// root bypasses the limit, but a CAP_NET_RAW-only run would not.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Printf("warning: cannot raise RLIMIT_MEMLOCK: %v", err)
	}

	if vlan != 0 {
		warnVLANOffload(iface)
	}

	// What the NIC can do for us. The transmit checksum request is only worth
	// making if the driver honours it; one that does not sends the frame as is.
	offload := false
	if feat, err := afxdp.QueryXSKFeatures(iface); err != nil {
		log.Printf("xsk features: %v (assuming none)", err)
	} else {
		log.Printf("xsk features: zero-copy %v, zc-max-segs %d, rx-metadata %#x, tx-checksum %v, tx-timestamp %v",
			feat.ZeroCopy(), feat.ZCMaxSegs, feat.RXMetadata, feat.TxChecksum, feat.TxTimestamp)
		offload = feat.TxChecksum
	}
	switch txCsumMode {
	case "auto":
	case "on":
		offload = true
	case "off":
		offload = false
	default:
		return nil, fmt.Errorf("-txcsum %q: want auto, on or off", txCsumMode)
	}
	log.Printf("transmit checksums: %s", map[bool]string{true: "NIC offload", false: "software"}[offload])

	// Only our port is taken; the kernel keeps ARP (and SSH, and the rest).
	// A load generator wants the other direction: everything *from* the
	// server's port.
	ports := portList(port)
	match := afxdp.MatchTCPPort(ports...)
	if clientStack {
		match = afxdp.MatchTCPSrcPort(ports...)
	}
	opts := []afxdp.Option{
		afxdp.WithFilter(match),
		afxdp.WithKeepManagement(),
		afxdp.WithNumFrames(rxFrames),
		afxdp.WithTxFrames(txFramesN),
		afxdp.WithRingSize(ringN),
	}
	if fleetTune.needWakeup {
		opts = append(opts, afxdp.WithNeedWakeup())
	}
	if fleetTune.busyPollUsecs > 0 {
		opts = append(opts, afxdp.WithBusyPoll(fleetTune.busyPollUsecs, 256))
	}
	if fleetTune.noAutoTune {
		opts = append(opts, afxdp.WithoutAutoTune())
	}
	if fleetTune.noAffinity {
		opts = append(opts, afxdp.WithoutAffinity())
	}
	if offload {
		opts = append(opts, afxdp.WithTxMetadata())
	}
	// Content served without a copy lives in one UMEM shared by every socket
	// (see xdplink/txcache.go); the cache takes its frames back as the NIC
	// completes them.
	var cache atomic.Pointer[xdplink.TxCache]
	if cacheBytes > 0 {
		opts = append(opts, afxdp.WithSharedMemory(cacheBytes),
			afxdp.WithOnForeignComplete(func(addr uint64) {
				if c := cache.Load(); c != nil {
					c.Release(addr)
				}
			}))
	}
	opts = append(opts, extra...)
	fleet, err := afxdp.Open(iface, opts...)
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", iface, err)
	}
	if cacheBytes > 0 {
		cacheMTU = int(mtu)
		c := xdplink.NewTxCache(fleet)
		if c == nil {
			fleet.Close()
			return nil, errors.New("netstack: no shared UMEM for the cache")
		}
		fillCache(c)
		c.SetPayloadLen(cachePayload(c))
		cache.Store(c)
		log.Printf("cache: %d frames of %d payload bytes (%d MiB) in the shared UMEM",
			c.Frames(), cachePayload(c), c.Frames()*cachePayload(c)>>20)
	}
	if info, err := fleet.Info(); err == nil {
		log.Printf("%s", info)
	}
	// A socket that is not zero-copy costs a copy per frame in softirq and
	// none of the README's numbers apply. The commonest reason on this rig
	// was an MTU larger than one frame receives (MaxPacket), which mlx5
	// refuses silently; say so, since the mode is one word in a long line.
	if socks := fleet.Sockets(); len(socks) > 0 {
		if zc, err := socks[0].ZeroCopy(); err == nil && !zc {
			info, _ := fleet.Info()
			switch {
			case strings.Contains(fmt.Sprint(info), "generic"):
				// asked for (tests on veth); nothing to say
			case ifi.MTU > socks[0].MaxPacket():
				log.Printf("WARNING: %s bound in copy mode: its MTU %d is more than the %d bytes one %d-byte frame receives, and the driver refuses zero-copy for it; ip link set %s mtu %d (or use larger frames)",
					iface, ifi.MTU, socks[0].MaxPacket(), socks[0].FrameSize(), iface, socks[0].MaxPacket())
			default:
				log.Printf("WARNING: %s bound in copy mode: the driver refused a zero-copy socket; throughput and CPU will not match the README", iface)
			}
		}
	}
	log.Printf("waiting for the link (a native XDP attach bounces it)...")
	if !fleet.WaitLinkUp(15 * time.Second) {
		log.Printf("warning: %s not up after 15s; running anyway", iface)
	}

	cfg := xdplink.Config{
		MAC: ifi.HardwareAddr, VLAN: vlan, MTU: mtu, TxMode: tm, GRO: useGRO, GSO: useGSO,
		// Deferred kicks need the sender to be the socket's own loop: true
		// per queue, and with several queues behind one stack only when the
		// fork transmits each connection on the queue it arrives on.
		DeferKicks: deferKicksFlag && (perQueue || forkTxHashFromRx()), TxChecksumOffload: offload, TxCache: cache.Load(), GROHold: groHoldFlag, TxInflight: txInflightFlag, TrustRxChecksums: rxCsumTrust, PinCPUs: pinCPUs,
	}
	socks := fleet.Sockets()
	groups := [][]*afxdp.Socket{socks}
	if perQueue {
		groups = make([][]*afxdp.Socket, len(socks))
		for i, xsk := range socks {
			groups[i] = []*afxdp.Socket{xsk}
		}
	}
	addr := tcpip.AddrFrom4(prefix.Addr().As4())
	rt := &netstackRuntime{fleet: fleet, addr: addr, port: port, cache: cache.Load()}
	clk := newFastClock() // shared: it has no mutable state, so one is plenty
	fail := func(what string, err error) (*netstackRuntime, error) {
		for _, u := range rt.units {
			if u.rtc != nil {
				u.rtc.stop()
			}
			for _, l := range u.lns {
				l.Close()
			}
			u.s.Destroy()
		}
		fleet.Close()
		return nil, fmt.Errorf("netstack: %s: %v", what, err)
	}
	for _, group := range groups {
		u := stackUnit{}
		ucfg := cfg
		if newRTC != nil {
			u.rtc = newRTC()
			if rtcSource {
				u.rtc.source = rt.cache // -mode source-zc streams the cache (rtc_zc.go)
			}
			u.rtc.sink = rtcSink
		}
		// After each receive batch, on the receive goroutine: first the TCP
		// work the fork set aside (handshakes, closes), then the
		// run-to-completion service of every connection made ready.
		stackp := new(*stack.Stack)
		rtc := u.rtc
		ucfg.AfterBatch = func() (more bool) {
			if *stackp != nil {
				forkDrainDeferred(*stackp)
			}
			if rtc != nil {
				more = rtc.drain()
			}
			return more
		}
		if tm == xdplink.TxDirect {
			// Under the fifo qdisc a refusal never reaches the sender (the
			// qdisc's goroutine drops what the link declines), so only the
			// direct mode can push back.
			ucfg.TxBacklogHigh, ucfg.TxBacklogLow = txBacklogHigh, txBacklogLow
		}
		ucfg.OnTxRoom = func() {
			if *stackp != nil {
				forkResumeBlocked(*stackp)
			}
		}
		ep, err := xdplink.NewForSockets(group, ucfg)
		if err != nil {
			return fail("endpoint", err)
		}
		u.ep = ep
		u.s = stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocolCUBIC},
			Clock:              clk,
		})
		*stackp = u.s
		rt.units = append(rt.units, u) // so fail() tears it down from here on
		if err := configureStack(u.s, ep, tm, addr, prefix.Bits()); err != nil {
			return fail("configure", err)
		}
		if u.rtc != nil {
			if err := u.rtc.listen(u.s, addr, ports); err != nil {
				return fail("listen", err)
			}
		} else if clientStack {
			// nothing to listen on
		} else {
			for _, p := range ports {
				ln, err := gonet.ListenTCP(u.s, tcpip.FullAddress{NIC: 1, Addr: addr, Port: p}, ipv4.ProtocolNumber)
				if err != nil {
					return fail("listen", err)
				}
				u.lns = append(u.lns, ln)
			}
			u.ln = u.lns[0]
		}
		rt.units[len(rt.units)-1] = u
	}
	rt.ep, rt.s, rt.ln = rt.units[0].ep, rt.units[0].s, rt.units[0].ln
	return rt, nil
}

// configureStack gives a fresh stack its NIC, address, route and the TCP
// options this example runs with.
func configureStack(s *stack.Stack, ep *xdplink.Endpoint, tm xdplink.TxMode, addr tcpip.Address, prefixBits int) error {
	nicOpts := stack.NICOptions{Name: "xdp0"}
	if tm == xdplink.TxFIFO {
		nicOpts.QDisc = fifo.New(ep, ep.NumQueues(), 1000)
	}
	if terr := s.CreateNICWithOptions(1, ep, nicOpts); terr != nil {
		return fmt.Errorf("create NIC: %v", terr)
	}
	if terr := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: prefixBits},
	}, stack.AddressProperties{}); terr != nil {
		return fmt.Errorf("add address: %v", terr)
	}
	// Everything is "on link": with no ARP in the stack there is nothing a
	// gateway entry could do, and the endpoint finds the MAC itself.
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	// A server that churns connections must be allowed to reuse ports held in
	// TIME-WAIT. Buffers start at 8 MiB (the kernel autotunes to a few MiB
	// on this link): a receive window that closes under bidirectional bulk
	// is what wedges a stack that follows RFC 793 literally, see patches/README.md (0004, now in 0012).
	tw := tcpip.TCPTimeWaitReuseOption(tcpip.TCPTimeWaitReuseGlobal)
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &tw); terr != nil {
		return fmt.Errorf("time-wait reuse: %v", terr)
	}
	if clientStack {
		// A load generator closes first, so TIME-WAIT is its problem: it
		// must come back to a source port within seconds, because only the
		// ports whose hash lands on this stack's queue may be used at all
		// (see rss.go). Nothing else in the lab reuses these four-tuples.
		twt := tcpip.TCPTimeWaitTimeoutOption(clientTimeWait)
		if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &twt); terr != nil {
			return fmt.Errorf("time-wait timeout: %v", terr)
		}
	}
	sb := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: sndBuf, Max: max(32<<20, sndBuf)}
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sb); terr != nil {
		return fmt.Errorf("send buffer: %v", terr)
	}
	rb := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: rcvBuf, Max: max(32<<20, rcvBuf)}
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rb); terr != nil {
		return fmt.Errorf("receive buffer: %v", terr)
	}
	if netstackRecovery >= 0 {
		rec := tcpip.TCPRecovery(netstackRecovery)
		if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rec); terr != nil {
			return fmt.Errorf("recovery: %v", terr)
		}
	}
	return nil
}

// stop tears down in dependency order: the listener, then the stack (which
// closes the qdisc, joins its goroutines, and detaches the endpoint, which
// joins its receive goroutines), and only then the fleet, whose Close unmaps
// the UMEM nothing references any more.
func (ns *netstackRuntime) stop() {
	for _, u := range ns.units {
		if u.rtc != nil {
			u.rtc.stop()
		}
		for _, l := range u.lns {
			l.Close()
		}
	}
	// Stop the receive loops before the stacks: Destroy holds the stack's
	// lock while it asks the link endpoint to stop, and a loop that is inside
	// the stack delivering a packet needs that lock to finish — under load
	// that is a deadlock. Stopped first, with no lock held, the loops finish
	// their batch and exit; Destroy then finds nothing to wait for.
	for _, u := range ns.units {
		u.ep.Close()
	}
	for _, u := range ns.units {
		u.s.Destroy()
	}
	ns.fleet.Close()
}

// totals aggregates the per-unit counters the report prints.
func (ns *netstackRuntime) totals() (es xdplink.Stats, t tcpTotals) {
	for _, u := range ns.units {
		x := u.ep.Stats()
		es.Rx += x.Rx
		es.Tx += x.Tx
		es.TxDeferred += x.TxDeferred
		es.TxDropped += x.TxDropped
		es.WrongVLAN += x.WrongVLAN
		es.NotIPv4 += x.NotIPv4
		es.Runt += x.Runt
		es.UnknownMAC += x.UnknownMAC
		es.RxBadCsum += x.RxBadCsum
		es.CsumOffloaded += x.CsumOffloaded
		es.CsumSoftware += x.CsumSoftware
		es.TxZeroCopy += x.TxZeroCopy
		es.TxRefused += x.TxRefused
		es.GSOPackets += x.GSOPackets
		es.GSOFrames += x.GSOFrames
		es.GROFrames += x.GROFrames
		es.GROPackets += x.GROPackets
		es.Neighbours += x.Neighbours
		ts := u.s.Stats().TCP
		t.segIn += ts.ValidSegmentsReceived.Value()
		t.segOut += ts.SegmentsSent.Value()
		t.established += ts.CurrentEstablished.Value()
		t.retx += ts.Retransmits.Value()
		t.csumErr += ts.ChecksumErrors.Value()
		t.invalid += ts.InvalidSegmentsReceived.Value()
		t.resetsSent += ts.ResetsSent.Value()
		t.resetsRcvd += ts.ResetsReceived.Value()
		t.timeouts += ts.Timeouts.Value()
		t.fastRetx += ts.FastRetransmit.Value()
		t.slowRetx += ts.SlowStartRetransmits.Value()
		t.dsack += ts.SegmentsAckedWithDSACK.Value()
		t.spuriousRTO += ts.SpuriousRTORecovery.Value()
		t.failed += ts.FailedConnectionAttempts.Value()
	}
	return es, t
}

type tcpTotals struct {
	segIn, segOut, established, retx, csumErr, invalid, resetsSent, resetsRcvd uint64
	timeouts, fastRetx, slowRetx, dsack, spuriousRTO, failed                   uint64
}

// server is the application: counters plus the two handlers.
type server struct {
	accepted, closed  atomic.Uint64
	bytesIn, bytesOut atomic.Uint64
	stopping          atomic.Bool // set before the listener is closed, so Accept errors are expected
	files             *fileSet    // -mode files
	sendfile          bool        // -mode files on the kernel: serve with sendfile(2)
}

// serve runs the handler for mode on ln until ln is closed.
func (s *server) serve(ln net.Listener, mode string) {
	switch mode {
	case "http":
		h := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Write([]byte("OK\n"))
			}),
			ConnState: func(c net.Conn, st http.ConnState) {
				switch st {
				case http.StateNew:
					s.accepted.Add(1)
				case http.StateClosed, http.StateHijacked:
					s.closed.Add(1)
				}
			},
		}
		if err := h.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
		}
	case "files":
		var el errLog
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				el.printf("accept: %v", err)
				continue
			}
			if s.sendfile {
				go s.serveFilesSendfile(c, s.files)
			} else {
				go s.serveFiles(c, s.files)
			}
		}
	case "source":
		var el errLog
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				el.printf("accept: %v", err)
				continue
			}
			s.accepted.Add(1)
			go s.source(c)
		}
	case "sink":
		var el errLog
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				el.printf("accept: %v", err)
				continue
			}
			s.accepted.Add(1)
			go s.sink(c)
		}
	case "echo", "echo-simple":
		simple := mode == "echo-simple"
		var el errLog
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) || s.stopping.Load() {
					return
				}
				el.printf("accept: %v", err)
				continue
			}
			s.accepted.Add(1)
			if simple {
				go s.echoSimple(c)
			} else {
				go s.echo(c)
			}
		}
	default:
		log.Fatalf("-mode %q: want echo or http", mode)
	}
}

// source writes continuously until the peer closes (one-way transmit test).
func (s *server) source(c net.Conn) {
	defer s.closed.Add(1)
	defer c.Close()
	buf := make([]byte, 64<<10)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	for {
		n, err := c.Write(buf)
		s.bytesOut.Add(uint64(n))
		if err != nil {
			return
		}
	}
}

// sink reads and discards until the peer closes (one-way receive test).
func (s *server) sink(c net.Conn) {
	defer s.closed.Add(1)
	defer c.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := c.Read(buf)
		s.bytesIn.Add(uint64(n))
		if err != nil {
			return
		}
	}
}

// echo writes back whatever it reads until the peer closes. Reading and
// writing run on separate goroutines joined by a bounded queue, so a write
// that blocks (the peer's window is full) does not stop the connection from
// being drained: with a single goroutine doing read-then-write, a stalled
// write shuts our own receive window and a bulk peer deadlocks with us.
// The queue blocks when full; nothing is dropped.
func (s *server) echo(c net.Conn) {
	defer s.closed.Add(1)
	defer c.Close()
	const depth = 128 // 8 MiB of slack, so a few ms of send-side stall never closes our receive window
	free := make(chan []byte, depth)
	full := make(chan []byte, depth)
	for i := 0; i < depth; i++ {
		free <- make([]byte, 64<<10)
	}
	go func() {
		defer close(full)
		for buf := range free {
			n, err := c.Read(buf[:cap(buf)])
			if n > 0 {
				s.bytesIn.Add(uint64(n))
				full <- buf[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	for b := range full {
		if _, err := c.Write(b); err != nil {
			// Drain so the reader can exit; Close above unblocks its Read.
			c.Close()
			for range full {
			}
			return
		}
		s.bytesOut.Add(uint64(len(b)))
		free <- b
	}
}

// echoSimple is the one-goroutine echo: read, write back, repeat. It is the
// simplest possible server and a legitimate pattern, but under bidirectional
// bulk a blocked write means nobody is reading; kept as -mode echo-simple to
// measure exactly that.
func (s *server) echoSimple(c net.Conn) {
	defer s.closed.Add(1)
	defer c.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			s.bytesIn.Add(uint64(n))
			if _, werr := c.Write(buf[:n]); werr != nil {
				return
			}
			s.bytesOut.Add(uint64(n))
		}
		if err != nil {
			return
		}
	}
}

// report logs once a second: the application's view on the left, and in
// netstack mode the rings and the stack on the right, so a gap between the
// two is visible. Never per packet.
func report(srv *server, ns *netstackRuntime) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last struct {
		acc, in, out, rx, tx, segIn, segOut uint64
		polls, kicks, suppressed, rxKicks   uint64
	}
	for range tick.C {
		acc, cl := srv.accepted.Load(), srv.closed.Load()
		in, out := srv.bytesIn.Load(), srv.bytesOut.Load()
		line := fmt.Sprintf("conns %d/s (active %d, total %d) | in %s out %s",
			acc-last.acc, acc-cl, acc, gbps(in-last.in), gbps(out-last.out))
		last.acc, last.in, last.out = acc, in, out
		if ns == nil {
			log.Print(line)
			continue
		}
		fs, _ := ns.fleet.Stats()
		es, ts := ns.totals()
		segIn, segOut := ts.segIn, ts.segOut
		log.Printf("%s | xdp rx %d/s tx %d/s | tcp seg in %d/s out %d/s est %d retx %d csum-err %d",
			line, fs.RxPackets-last.rx, fs.TxPackets-last.tx,
			segIn-last.segIn, segOut-last.segOut,
			ts.established, ts.retx, ts.csumErr)
		tlp, rackLost := forkProbeCounts()
		log.Printf("  tcp: invalid %d resetsSent %d resetsRcvd %d timeouts %d fastRetx %d slowRetx %d dsack %d spuriousRTO %d failedAttempts %d tlp %d rackLost %d",
			ts.invalid, ts.resetsSent, ts.resetsRcvd, ts.timeouts, ts.fastRetx, ts.slowRetx, ts.dsack, ts.spuriousRTO, ts.failed, tlp, rackLost)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		log.Printf("  mem: rss %s heap %s (sys %s) | goroutines %d",
			bytesHuman(processRSS()), bytesHuman(ms.HeapAlloc), bytesHuman(ms.Sys), runtime.NumGoroutine())
		log.Printf("  ep: rx %d tx %d txDeferred %d txRefused %d txDropped %d wrongVLAN %d notIPv4 %d runt %d unknownMAC %d badCsum %d | fleet rxDrop %d ringFull %d",
			es.Rx, es.Tx, es.TxDeferred, es.TxRefused, es.TxDropped, es.WrongVLAN, es.NotIPv4, es.Runt, es.UnknownMAC, es.RxBadCsum,
			fs.RxDropped, fs.RxRingFull)
		last.rx, last.tx, last.segIn, last.segOut = fs.RxPackets, fs.TxPackets, segIn, segOut
		log.Printf("  offload: gso %d pkts -> %d frames (%.1f/pkt) | gro %d frames -> %d pkts (%.1f/pkt) | tx csum nic %d sw %d | tx zero-copy %d",
			es.GSOPackets, es.GSOFrames, ratio(es.GSOFrames, es.GSOPackets),
			es.GROFrames, es.GROPackets, ratio(es.GROFrames, es.GROPackets), es.CsumOffloaded, es.CsumSoftware, es.TxZeroCopy)
		var polls, kicks, suppressed, rxKicks uint64
		for _, q := range fs.PerQueue {
			polls += q.Polls
			kicks += q.Kicks
			suppressed += q.KicksSuppressed
			rxKicks += q.RxKicks
		}
		// Counters restart from zero when the fleet closes under us, so a
		// delta is only a rate while it is not negative.
		rate := func(cur, prev uint64) uint64 {
			if cur < prev {
				return 0
			}
			return cur - prev
		}
		var re rtcErrors
		for _, u := range ns.units {
			if u.rtc != nil {
				x := u.rtc.errors()
				re.queueDropped += x.queueDropped
				re.zeroWindow += x.zeroWindow
				re.csumErr += x.csumErr
			}
		}
		{
			var dropped, malformed, invalid, badL4, ackDrop, rstBad uint64
			for _, u := range ns.units {
				st := u.s.Stats()
				dropped += st.DroppedPackets.Value()
				malformed += st.IP.MalformedPacketsReceived.Value()
				invalid += st.TCP.InvalidSegmentsReceived.Value()
				badL4 += st.NICs.MalformedL4RcvdPackets.Value()
				ackDrop += st.TCP.SegmentsAckedWithDSACK.Value()
				rstBad += st.TCP.ResetsReceived.Value()
			}
			if dropped+malformed+invalid+badL4 > 0 {
				log.Printf("  stack drops: dropped %d ip-malformed %d tcp-invalid %d l4-malformed %d (dsack-acked %d resets %d)",
					dropped, malformed, invalid, badL4, ackDrop, rstBad)
			}
		}
		if n := forkOutOfOrder(); n > 0 {
			log.Printf("  tcp receive: %d segments arrived out of order (queued for reassembly)", n)
		}
		if re != (rtcErrors{}) {
			log.Printf("  endpoint rx errors: segment queue dropped %d  zero-window episodes %d  csum %d", re.queueDropped, re.zeroWindow, re.csumErr)
		}
		log.Printf("  syscalls: polls %d/s  tx kicks %d/s (suppressed %d/s)  rx wakeups %d/s",
			rate(polls, last.polls), rate(kicks, last.kicks), rate(suppressed, last.suppressed), rate(rxKicks, last.rxKicks))
		last.polls, last.kicks, last.suppressed, last.rxKicks = polls, kicks, suppressed, rxKicks
		last.rx, last.tx, last.segIn, last.segOut = fs.RxPackets, fs.TxPackets, segIn, segOut

	}
}

// ratio is a/b, or 0 when b is 0.
func ratio(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// gbps formats a bytes-per-second count as bits per second.
func gbps(bytesPerSec uint64) string {
	bits := float64(bytesPerSec) * 8
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bits/1e6)
	default:
		return fmt.Sprintf("%.0f kbit/s", bits/1e3)
	}
}

// errLog prints at most one line per second, so a fault on the accept path
// cannot turn the log into the bottleneck.
type errLog struct{ last time.Time }

func (l *errLog) printf(format string, args ...any) {
	if now := time.Now(); now.Sub(l.last) >= time.Second {
		l.last = now
		log.Printf(format, args...)
	}
}

// warnVLANOffload says so when the NIC would strip 802.1Q tags before XDP
// sees them: every frame then arrives untagged and the endpoint drops it as
// the wrong VLAN, which reads as a silent black hole. ixgbe defaults to on.
func warnVLANOffload(iface string) {
	out, err := exec.Command("ethtool", "-k", iface).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "rx-vlan-offload:" && f[1] == "on" {
			log.Printf("WARNING: %s has rx-vlan-offload on: the NIC strips VLAN tags before XDP sees them, "+
				"so nothing tagged will reach the stack. Run: ethtool -K %s rxvlan off txvlan off rx-vlan-filter off", iface, iface)
		}
	}
}

// cachePayload is how much of each cache frame source-zc streams: exactly
// what netstack puts in one segment on this MTU — the MSS less the 40 bytes
// it reserves for TCP options (timestamps and SACK blocks) — so that a view
// is a segment and the sender never splits one mid-frame.
func cachePayload(c *xdplink.TxCache) int { return min(c.PayloadCap(), cacheMTU-40-40) }

// fillCache writes a recognisable pattern: frame i, byte j holds
// byte(i*7 + j). The tests check it on the far side.
func fillCache(c *xdplink.TxCache) {
	n := cachePayload(c)
	for i := 0; i < c.Frames(); i++ {
		p := c.Payload(i)[:n]
		for j := range p {
			p[j] = byte(i*7 + j)
		}
	}
}

// processRSS is the process's resident size, for the per-connection memory
// question: an endpoint, its buffers and its timers are the cost of holding
// a connection open, and only the whole process shows it.
func processRSS() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb << 10
	}
	return 0
}

// bytesHuman prints a byte count the way an operator reads one.
func bytesHuman(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
