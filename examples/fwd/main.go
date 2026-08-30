//go:build linux

// fwd is a multi-queue IPv4 forwarder: it takes the packets steered to it,
// decrements the TTL, rewrites the Ethernet header for a fixed next hop, and
// transmits each packet back out the same interface — in the frame it arrived
// in. One socket and one goroutine per queue, like drop; zero copies.
//
//	go build -o fwd ./examples/fwd
//	sudo ./fwd -iface eth0 -port 9999 -next-hop 02:00:00:00:00:01
//
// The zero-copy hairpin is WithTxReuseRxFrames: a received frame is handed
// straight to Transmit, and Complete routes it back to the receive pool it
// came from. That is the pattern a real dataplane wants; examples/l2fwd shows
// the copying alternative on a single queue.
//
// The forwarding decision here is deliberately fixed (every packet goes to
// -next-hop) so what is measured is the packet path, not a routing table.
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atoonk/go-afxdp"
)

func main() {
	iface := flag.String("iface", "eth0", "interface to bind")
	port := flag.Int("port", 9999, "forward packets arriving on this UDP port")
	all := flag.Bool("all", false, "forward every packet instead of just -port")
	queues := flag.Int("queues", 0, "rx queues to bind (0 = all)")
	nextHop := flag.String("next-hop", "", "destination MAC for forwarded packets (required)")
	srcMAC := flag.String("src-mac", "", "source MAC on forwarded packets (default: the interface's own)")
	noTune := flag.Bool("no-autotune", false, "leave the interface's NAPI settings alone")
	noAffinity := flag.Bool("no-affinity", false, "leave CPU placement to the scheduler")
	cpus := flag.String("cpus", "", "run the workers on these processors, e.g. 4,6,8 or 4-7")
	flag.Parse()

	if *nextHop == "" {
		log.Fatal("-next-hop is required: the MAC forwarded packets are sent to")
	}
	dst, err := net.ParseMAC(*nextHop)
	if err != nil || len(dst) != 6 {
		log.Fatalf("-next-hop %q is not an Ethernet address", *nextHop)
	}
	var src net.HardwareAddr
	if *srcMAC != "" {
		if src, err = net.ParseMAC(*srcMAC); err != nil || len(src) != 6 {
			log.Fatalf("-src-mac %q is not an Ethernet address", *srcMAC)
		}
	} else {
		ifi, err := net.InterfaceByName(*iface)
		if err != nil || len(ifi.HardwareAddr) != 6 {
			log.Fatalf("no Ethernet address on %s; pass -src-mac", *iface)
		}
		src = ifi.HardwareAddr
	}

	filter := afxdp.WithUDPPorts(uint16(*port))
	if *all {
		filter = afxdp.WithFilter(afxdp.MatchAll())
	}
	// TxReuseRxFrames is what makes this zero-copy: received frames go out as
	// they are and come home to the receive pool on completion.
	opts := []afxdp.Option{filter, afxdp.WithQueues(*queues), afxdp.WithTxReuseRxFrames()}
	if *noTune {
		opts = append(opts, afxdp.WithoutAutoTune())
	}
	if *noAffinity && *cpus != "" {
		log.Fatal("-no-affinity and -cpus contradict each other: pick one")
	}
	if *noAffinity {
		opts = append(opts, afxdp.WithoutAffinity())
	} else if *cpus != "" {
		opts = append(opts, afxdp.WithAffinity(parseCPUs(*cpus)...))
	}
	fleet, err := afxdp.Open(*iface, opts...)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	if info, err := fleet.Info(); err == nil {
		log.Printf("forwarding to %s: %s", dst, info)
	}

	log.Printf("waiting for the link to come up (native XDP attach bounces it, ~10s on some NICs)...")
	if fleet.WaitLinkUp(15 * time.Second) {
		log.Printf("link up")
	} else {
		log.Printf("warning: %s not up after 15s; forwarding anyway", *iface)
	}

	var d6, s6 [6]byte
	copy(d6[:], dst)
	copy(s6[:], src)
	var fwd, dropped atomic.Uint64
	for _, xsk := range fleet.Sockets() {
		go forward(xsk, d6, s6, &fwd, &dropped)
	}
	time.Sleep(200 * time.Millisecond)
	log.Printf("cpu affinity: %v", fleet.Affinity())

	go report(&fwd, &dropped)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fleet.Close()
	log.Println("stopped")
}

// forward is one queue's loop: reclaim what the NIC finished sending, keep the
// kernel supplied with receive buffers, wait for packets, rewrite each in
// place and transmit it from the frame it arrived in.
func forward(xsk *afxdp.Socket, dst, src [6]byte, fwd, dropped *atomic.Uint64) {
	out := make([]afxdp.Desc, 0, 256)
	drop := make([]afxdp.Desc, 0, 256)
	for {
		xsk.Complete(xsk.NumCompleted())
		xsk.Fill(xsk.NumFreeFillSlots())
		// The poll must be bounded, unlike drop's: in the hairpin every
		// receive frame can be in flight on transmit at once, and a forever
		// poll would then wait for packets that cannot arrive (the fill ring
		// is empty) while the completions that would refill it sit unread
		// above. A bounded poll always comes back to Complete.
		n, err := xsk.Poll(100 * time.Millisecond)
		if err != nil {
			return // closed
		}
		rx := xsk.Receive(n)
		if len(rx) == 0 {
			continue
		}
		out, drop = out[:0], drop[:0]
		for _, d := range rx {
			if rewrite(xsk.GetFrame(d), dst, src) {
				out = append(out, d)
			} else {
				drop = append(drop, d)
			}
		}
		sent := 0
		if len(out) > 0 {
			sent = xsk.Transmit(out)
			// A full tx ring is a drop, not something to spin on; the next
			// Complete at the top of the loop keeps the ring draining.
			drop = append(drop, out[sent:]...)
		}
		xsk.Recycle(drop)
		fwd.Add(uint64(sent))
		dropped.Add(uint64(len(rx) - sent))
	}
}

// rewrite turns a received frame into the forwarded one, in place: new
// Ethernet addresses, TTL down one, checksum updated incrementally. Returns
// false for anything that is not forwardable IPv4 (which the caller drops,
// like a router would).
func rewrite(f []byte, dst, src [6]byte) bool {
	if len(f) < 34 {
		return false
	}
	ip := 14
	et := binary.BigEndian.Uint16(f[12:14])
	if et == 0x8100 { // 802.1Q: the IPv4 header sits one tag further in
		if len(f) < 38 {
			return false
		}
		et = binary.BigEndian.Uint16(f[16:18])
		ip = 18
	}
	if et != 0x0800 || f[ip]>>4 != 4 {
		return false
	}
	ttl := f[ip+8]
	if ttl <= 1 {
		return false
	}
	f[ip+8] = ttl - 1
	// RFC 1141: the TTL|protocol word went down by 0x0100, so the checksum
	// goes up by it, with the carry folded back in.
	c := uint32(binary.BigEndian.Uint16(f[ip+10:ip+12])) + 0x0100
	binary.BigEndian.PutUint16(f[ip+10:ip+12], uint16(c+(c>>16)))
	copy(f[0:6], dst[:])
	copy(f[6:12], src[:])
	return true
}

func report(fwd, dropped *atomic.Uint64) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastF, lastD uint64
	for range t.C {
		fp, dp := fwd.Load(), dropped.Load()
		log.Printf("%d fwd pps   %d dropped/s", fp-lastF, dp-lastD)
		lastF, lastD = fp, dp
	}
}

// parseCPUs turns "4,6,8" or "4-7" (or a mix) into a list of processors.
func parseCPUs(s string) []int {
	var cpus []int
	for _, part := range splitComma(s) {
		lo, hi, ok := parseRange(part)
		if !ok {
			log.Fatalf("-cpus: %q is not a processor or a range", part)
		}
		for c := lo; c <= hi; c++ {
			cpus = append(cpus, c)
		}
	}
	return cpus
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i + 1
		}
	}
	return parts
}

func parseRange(s string) (lo, hi int, ok bool) {
	dash := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			dash = i
			break
		}
	}
	atoi := func(t string) (int, bool) {
		if t == "" {
			return 0, false
		}
		n := 0
		for i := 0; i < len(t); i++ {
			if t[i] < '0' || t[i] > '9' {
				return 0, false
			}
			n = n*10 + int(t[i]-'0')
		}
		return n, true
	}
	if dash < 0 {
		n, good := atoi(s)
		return n, n, good
	}
	lo, ok1 := atoi(s[:dash])
	hi, ok2 := atoi(s[dash+1:])
	return lo, hi, ok1 && ok2 && lo <= hi
}
