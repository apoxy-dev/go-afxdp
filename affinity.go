//go:build linux

// Copyright 2024 Andree Toonk. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package afxdp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Auto-affinity: run each queue's worker on the same core as that queue's NIC
// interrupt.
//
// An AF_XDP queue is really two halves. The kernel half — the hard interrupt,
// the NAPI poll, the XDP program, the redirect into the socket — runs wherever
// that queue's interrupt is routed. The userspace half — Poll, Receive,
// SendFunc — runs wherever the Go scheduler happens to put the goroutine.
// Left alone those two halves land on unrelated cores, and every packet then
// crosses a cache boundary: the descriptor rings, the UMEM frames and the
// socket state are all written by one core and read by the other.
//
// Measured on a 100G ConnectX-6 Dx (EPYC 9275F, 24 physical cores in L3
// complexes of 3, SMT siblings n and n+24), one transmit worker driving one
// queue whose interrupt is on core 0:
//
//	worker on core 0  (the interrupt's own core)   15.0 Mpps   1.01 cores
//	worker on core 24 (its SMT sibling)            16.2 Mpps   2.00 cores
//	worker on core 1  (same L3 complex)            16.3 Mpps   2.00 cores
//	worker on core 12 (a different complex)         6.2 Mpps   2.01 cores
//
// The dominant effect is the last line: leaving the L3 complex costs 2.6x the
// throughput for the same CPU, and an unplaced goroutine on a 24-core chiplet
// CPU lands outside the right complex almost every time. Whole-machine
// transmit and receive, same binary, same load, the only difference being
// whether the library places the workers:
//
//	           transmitted             kept of 132 Mpps offered
//	queues   unplaced      placed      unplaced      placed
//	1          6.5  2.0c    14.3  1.0c   34.0  1.5c    35.5  1.5c
//	4         33.0  8.0c    59.6  4.0c  125.9  6.2c   129.5  6.3c
//	8         55.2 16.0c   118.7  8.0c   45.6  2.2c   129.6  9.2c
//	16       112.2 32.1c   147.8 16.0c   66.8  3.7c   132.0  9.6c
//	48            —              —      129.9 10.3c   130.5 13.8c
//
// Twice the packets for half the CPU transmitting — 1300 cycles per packet down
// to 290, and 64-byte line rate on eight physical cores — and everything
// offered, with no drops at all, receiving at every queue count. The 132 Mpps
// in the receive columns is the generator's ceiling rather than the receiver's:
// offered a full 148.7 Mpps it keeps 148.5 on 9.4 cores from eight queues, and
// 148.4 from thirty-two, still without dropping anything.
//
// A fleet that can receive is placed *next to* its interrupts rather than on
// them. Sharing the CPU is the best possible arrangement for transmit, but it
// is catastrophic for receive, where the kernel half is far more expensive:
// NAPI, the XDP program and the redirect need a whole CPU of their own at
// 30 Mpps, and a receive loop asked to share with them gets almost no time at
// all. One queue, 131 Mpps offered:
//
//	worker next to the interrupt   30.5 Mpps kept   1.50 cores
//	worker on the interrupt's CPU   0.99 Mpps kept  1.01 cores (0.99 softirq)
//
// That is a thirtyfold collapse, not a busy core, so for those fleets the rule
// is: never share. Preference order for a worker, in the order the code tries
// them:
//
//  1. another physical core in the complex the interrupt is already on, leaving
//     the interrupt exactly where the driver's hints put it;
//  2. failing that, any complex with two physical cores to spare — both halves
//     move there together;
//  3. failing that, one free core with the worker on its SMT sibling. The two
//     then share execution units, which costs a receiver about a third of its
//     packets, but beats being in the wrong complex by an order of magnitude.
//
// A queue with nowhere left to go is left to the scheduler rather than stacked
// on top of its interrupt. Steps 2 and 3 will cross to a far NUMA node if the
// near one is full: the node ordering is a preference, not a boundary.
//
// A fleet that redirects nothing — a generator, WithFilter(MatchNone()) — never
// receives, so that objection does not apply and it shares instead. Sharing is
// what lets one core carry a whole queue rather than half of one, and on a
// generator that is the difference between filling a link and not:
//
//	queues   unplaced            sharing
//	1         6.5 Mpps 2.0c      14.3 Mpps 1.0c
//	4        33.0 Mpps 8.0c      59.6 Mpps 4.0c
//	8        55.2 Mpps 16.0c    118.7 Mpps 8.0c
//	16      112.2 Mpps 32.1c    147.8 Mpps 16.0c   (99.3 Gbit/s, line rate)
//	24      145.9 Mpps 45.9c    148.5 Mpps 24.1c
//
// Line rate at 64 bytes on sixteen logical CPUs — eight physical cores, since
// the pairs are SMT siblings — where the unplaced library needed thirty-two to
// reach 112 Mpps.
//
// If the interrupts themselves are somewhere unusable — irqbalance has
// scattered them, or several queues share one core — they are moved to a core
// of their own first, and put back on Close. The sharing arrangement is the
// exception: it seats workers on the interrupts as it finds them and never
// steers, so a queue whose interrupt is scattered is simply left alone. See
// WithoutAffinity.

// affinityPlan is one Fleet's queue-to-CPU assignment. It is built by Open,
// shared (read-only after construction, except for the pinned bookkeeping)
// with every Socket in the Fleet, and released by Close.
type affinityPlan struct {
	iface string
	cpu   []int   // per queue: CPU to run the worker on; -1 if unknown
	irqs  [][]int // per queue: the interrupts belonging to it
	note  string  // why we could not do better, for Info

	// held is the interrupts this plan rerouted, and so the only ones it may
	// put back. Knowing an interrupt's number is not the same as having moved
	// it: a plan that shares interrupt cores never steers at all, and steering
	// skips an interrupt already sitting where it wants it — which is exactly
	// the state another fleet's steering leaves behind. Releasing on the
	// strength of p.irqs would let one fleet undo another's placement and
	// leave its workers beside an interrupt that has moved away.
	held []int
	// failed counts interrupts we meant to move and could not, which on a
	// native-mode NIC almost always means the process is not root.
	failed int

	mu     sync.Mutex
	pinned map[int]int // queue -> CPU actually pinned, for Info/tests
}

// lockOSThread and unlockOSThread are indirected so tests can exercise the
// pinning logic without wiring goroutines to threads for the rest of the run.
var (
	lockOSThread   = runtime.LockOSThread
	unlockOSThread = runtime.UnlockOSThread
)

// buildAffinityPlan works out where each queue's two halves belong.
//
// want, if non-nil, is the caller's explicit CPU list (WithAffinity): queue q
// runs on want[q % len(want)] and its interrupt is brought next to it.
//
// Otherwise the plan seats each queue's interrupt and its worker on two
// different physical cores of one cache complex, preferring to leave the
// interrupt exactly where the driver's affinity hints put it and only moving it
// when its complex has no second core to spare. Queues that find no room are
// left to the scheduler: a worker stacked on top of its own interrupt receives
// almost nothing (see the commentary above), which is worse than not placing it.
//
// Like the NAPI tuning this is entirely best-effort: anything it cannot read or
// write leaves that queue unpinned and the reason in the plan's note, because
// affinity is an optimisation and never a reason for Open to fail.
func buildAffinityPlan(iface string, nQueues int, want []int, transmitOnly bool) *affinityPlan {
	p := &affinityPlan{
		iface:  iface,
		cpu:    make([]int, nQueues),
		irqs:   make([][]int, nQueues),
		pinned: make(map[int]int),
	}
	for q := range p.cpu {
		p.cpu[q] = -1
	}

	byQueue, err := queueIRQs(iface)
	if err != nil || len(byQueue) == 0 {
		// No interrupts we can attribute to a queue (a virtual device, an
		// unfamiliar driver naming scheme, or /proc/irq not readable). There is
		// nothing to sit next to, so an explicit CPU list is still honoured and
		// everything else is left alone.
		p.note = "no per-queue interrupts found"
		if want != nil {
			for q := range p.cpu {
				p.cpu[q] = want[q%len(want)]
			}
			p.note = "explicit cpus, no per-queue interrupts found"
		}
		return p
	}
	for q := 0; q < nQueues; q++ {
		p.irqs[q] = byQueue[q]
	}

	// A fleet that redirects nothing never receives, so the reason for keeping
	// a worker off its interrupt's CPU — the receive softirq needing that CPU
	// to itself — does not apply. Sharing is then the best arrangement there
	// is: one queue's two halves on one logical CPU, transmitting 14.1 Mpps,
	// against 16.2 Mpps for the same queue spread over two. Nearly the same
	// packets for half the CPU, which on a generator is the whole point — it
	// is what lets every core carry two queues instead of one.
	if transmitOnly && want == nil {
		taken := make(map[int]bool)
		for q := 0; q < nQueues; q++ {
			cpu, ok := singleCPU(p.irqs[q])
			if !ok || taken[cpu] {
				continue // scattered interrupts, or two queues on one core
			}
			p.cpu[q] = cpu
			taken[cpu] = true
		}
		p.note = "transmit only, sharing interrupt cores"
		return p
	}

	topo := readTopology(iface)
	irqCPU := make([]int, nQueues)
	for q := range irqCPU {
		irqCPU[q] = -1
	}

	if want != nil {
		// The caller has named the CPUs for the workers; our job is to bring
		// each queue's interrupt next to its worker — beside it, not on it.
		for q := 0; q < nQueues; q++ {
			p.cpu[q] = want[q%len(want)]
			topo.take(p.cpu[q])
		}
		for q := 0; q < nQueues; q++ {
			if len(p.irqs[q]) == 0 {
				continue
			}
			if cpu, ok := topo.freeNear(p.cpu[q]); ok {
				irqCPU[q] = cpu
				topo.take(cpu)
			}
		}
		p.steerIRQs(irqCPU)
		p.note = "explicit cpus"
		if p.failed > 0 {
			p.note = fmt.Sprintf("explicit cpus, %d interrupts could not be steered (not root?)", p.failed)
		}
		return p
	}

	// Reserve every physical core a bound queue's interrupt already sits on,
	// so a worker is never seated on a core the kernel is about to do queue
	// work on. Without this, placing queue 0 can hand queue 3's interrupt core
	// to queue 0's worker — the stacked arrangement measured above at 0.99
	// Mpps — and whether queue 3 later escapes depends on there being room
	// left to move it to. Only bound queues count: interrupts for channels
	// nobody is using stay idle and cost nothing.
	current := make([]int, nQueues)
	for q := 0; q < nQueues; q++ {
		current[q] = -1
		if cpu, ok := singleCPU(p.irqs[q]); ok {
			current[q] = cpu
			topo.take(cpu)
		}
	}

	moved := 0
	for q := 0; q < nQueues; q++ {
		if len(p.irqs[q]) == 0 {
			continue
		}
		// First choice: leave the interrupt where the driver put it, and seat
		// the worker on another core of the same complex. Touching nothing is
		// always better than touching something, and driver hints are already
		// one interrupt per core on the NIC's own node. This queue's own
		// interrupt core is released first so it can be considered as the
		// pair's interrupt half, but never as the worker half.
		if cur := current[q]; cur >= 0 {
			topo.release(cur)
			if w, ok := topo.freeInComplexOf(cur); ok {
				irqCPU[q], p.cpu[q] = cur, w
				topo.take(cur)
				topo.take(w)
				continue
			}
			topo.take(cur) // still an interrupt core; keep it reserved
		}
		// Second choice: any complex with two physical cores to spare. The
		// interrupt moves out from under whatever it was sharing with.
		if a, b, ok := topo.freePairInAComplex(); ok {
			if cur := current[q]; cur >= 0 {
				topo.release(cur) // it is leaving that core
			}
			irqCPU[q], p.cpu[q] = a, b
			topo.take(a)
			topo.take(b)
			moved++
			continue
		}
		// Third choice: one core to spare, with the worker on its SMT sibling.
		// The two halves then share execution units, which costs a receiver
		// about a third of its packets, but it is still an order of magnitude
		// better than being in the wrong cache complex.
		if a, sib, ok := topo.freeCoreWithSibling(); ok {
			if cur := current[q]; cur >= 0 {
				topo.release(cur)
			}
			irqCPU[q], p.cpu[q] = a, sib
			topo.take(a)
			moved++
			continue
		}
		// Out of room. Leave this queue to the scheduler rather than stack it.
	}

	// An interrupt we could not move is an interrupt still where it was, so
	// the worker we placed for it is now beside the wrong core. Unplace those
	// queues: no placement beats a confidently wrong one.
	if failed := p.steerIRQs(irqCPU); len(failed) > 0 {
		for q := range failed {
			p.cpu[q] = -1
		}
	}

	placed := 0
	for _, cpu := range p.cpu {
		if cpu >= 0 {
			placed++
		}
	}
	switch {
	case p.failed > 0:
		p.note = fmt.Sprintf("%d interrupts could not be steered (not root?)", p.failed)
	case placed == 0:
		p.note = "no room to place any queue"
	case placed < nQueues:
		p.note = fmt.Sprintf("%d of %d queues had no room", nQueues-placed, nQueues)
	case moved == 0:
		p.note = "following driver hints"
	}
	return p
}

// topology is the machine's CPUs grouped the way placement cares about them:
// physical cores (an SMT group counts once) collected into cache complexes,
// with the NIC's own NUMA node first. taken marks a physical core as spoken
// for, by either half of some queue.
type topology struct {
	complexes [][]int       // physical-lead CPUs per last-level cache, best first
	siblings  map[int][]int // physical lead -> every logical CPU on that core
	leadOf    map[int]int   // any logical CPU -> its physical lead
	complexOf map[int]int   // physical lead -> index into complexes
	taken     map[int]bool  // physical leads no longer available
}

// readTopology reads the CPU layout from sysfs. A machine that reports no cache
// topology (some VMs) comes back as a single complex, which degrades placement
// to "anywhere on the right node" rather than breaking it.
func readTopology(iface string) *topology {
	t := &topology{
		siblings:  make(map[int][]int),
		leadOf:    make(map[int]int),
		complexOf: make(map[int]int),
		taken:     make(map[int]bool),
	}
	// Only CPUs this process may actually run on. Without this a process
	// confined by taskset, a cpuset cgroup or a container would plan for cores
	// it cannot use, fail every pin — and, worse, still move the host's
	// interrupts onto those cores on the way. The confinement has to bind the
	// host state we touch, not just our own threads.
	var allowed unix.CPUSet
	haveAllowed := unix.SchedGetaffinity(0, &allowed) == nil

	local := make(map[int]bool)
	if node := deviceNUMANode(iface); node >= 0 {
		for _, c := range readCPUList(fmt.Sprintf("/sys/devices/system/node/node%d/cpulist", node)) {
			local[c] = true
		}
	}
	// Group physical cores by the CPU set they share their last-level cache
	// with, keyed by the lowest CPU in that set so the grouping is stable.
	byCache := make(map[int][]int)
	var order []int
	for _, c := range onlineCPUs() {
		if haveAllowed && !allowed.IsSet(c) {
			continue
		}
		sibs := readCPUList(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", c))
		if len(sibs) == 0 {
			sibs = []int{c}
		}
		// With CPUs filtered out above, the kernel's first sibling may not be
		// one we are allowed; treat the lowest allowed sibling as the lead so
		// a core is still counted exactly once.
		lead := c
		var usable []int
		for _, sib := range sibs {
			if !haveAllowed || allowed.IsSet(sib) {
				usable = append(usable, sib)
			}
		}
		if len(usable) > 0 {
			lead = usable[0]
		} else {
			usable = []int{c}
		}
		t.leadOf[c] = lead
		if c != lead {
			continue // counted once, under its lead
		}
		t.siblings[c] = usable
		key := c
		if llc := sharedCacheCPUs(c); len(llc) > 0 {
			key = llc[0]
		}
		if _, seen := byCache[key]; !seen {
			order = append(order, key)
		}
		byCache[key] = append(byCache[key], c)
	}
	// The NIC's own node first: a complex on the far node is worse than any
	// arrangement on the near one.
	sort.SliceStable(order, func(i, j int) bool {
		li, lj := local[order[i]], local[order[j]]
		if li != lj {
			return li
		}
		return false
	})
	for _, key := range order {
		t.complexes = append(t.complexes, byCache[key])
	}
	for i, cores := range t.complexes {
		for _, c := range cores {
			t.complexOf[c] = i
		}
	}
	return t
}

// take reserves the physical core a CPU belongs to.
func (t *topology) take(cpu int) {
	if lead, ok := t.leadOf[cpu]; ok {
		t.taken[lead] = true
	}
}

// release makes a physical core available again, for when a queue's interrupt
// is about to leave the core it was reserved on.
func (t *topology) release(cpu int) {
	if lead, ok := t.leadOf[cpu]; ok {
		delete(t.taken, lead)
	}
}

// coreFree reports whether the physical core a CPU belongs to is unclaimed.
func (t *topology) coreFree(cpu int) bool {
	lead, ok := t.leadOf[cpu]
	return ok && !t.taken[lead]
}

// freeInComplexOf returns a free physical core sharing cpu's last-level cache.
// cpu's own core is never returned: it is either taken, or the caller released
// it precisely because something else is about to occupy it.
func (t *topology) freeInComplexOf(cpu int) (int, bool) {
	lead, ok := t.leadOf[cpu]
	if !ok {
		return 0, false
	}
	idx, ok := t.complexOf[lead]
	if !ok {
		return 0, false
	}
	for _, c := range t.complexes[idx] {
		if c != lead && !t.taken[c] {
			return c, true
		}
	}
	return 0, false
}

// freePairInAComplex returns two free physical cores sharing one cache.
func (t *topology) freePairInAComplex() (int, int, bool) {
	for _, cores := range t.complexes {
		var free []int
		for _, c := range cores {
			if !t.taken[c] {
				free = append(free, c)
				if len(free) == 2 {
					return free[0], free[1], true
				}
			}
		}
	}
	return 0, 0, false
}

// freeCoreWithSibling returns a free physical core and a second logical CPU on
// it, for when there is no longer a whole core to spare for each half.
func (t *topology) freeCoreWithSibling() (int, int, bool) {
	for _, cores := range t.complexes {
		for _, c := range cores {
			if !t.taken[c] && len(t.siblings[c]) > 1 {
				return c, t.siblings[c][1], true
			}
		}
	}
	return 0, 0, false
}

// freeNear returns the nearest free physical core to cpu: its own complex
// first, then anywhere. Used for the explicit-CPU path, where the workers are
// already placed and the interrupts have to find room around them.
func (t *topology) freeNear(cpu int) (int, bool) {
	if c, ok := t.freeInComplexOf(cpu); ok {
		return c, true
	}
	for _, cores := range t.complexes {
		for _, c := range cores {
			if !t.taken[c] {
				return c, true
			}
		}
	}
	return 0, false
}

// sharedCacheCPUs returns the CPUs sharing the largest cache level with cpu —
// the CCX on chiplet designs, the whole socket on monolithic ones.
func sharedCacheCPUs(cpu int) []int {
	var best []int
	for level := 0; level <= 4; level++ {
		list := readCPUList(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cache/index%d/shared_cpu_list", cpu, level))
		if len(list) > len(best) {
			best = list
		}
	}
	return best
}

// steerIRQs points every queue's interrupts at the CPU chosen for them and
// records what it took, so release can put back that and nothing else. It
// reports which queues it could not steer.
//
// An interrupt already sitting on the target is still claimed rather than
// skipped: setIRQAffinity is idempotent and refcounted, and taking the
// reference is what stops another fleet from restoring it underneath us.
func (p *affinityPlan) steerIRQs(target []int) (failedQueues map[int]bool) {
	for q, cpu := range target {
		if cpu < 0 || q >= len(p.irqs) {
			continue
		}
		for _, irq := range p.irqs[q] {
			if !setIRQAffinity(irq, cpu) {
				if failedQueues == nil {
					failedQueues = make(map[int]bool)
				}
				failedQueues[q] = true
				p.failed++
				continue
			}
			p.held = append(p.held, irq)
		}
	}
	return failedQueues
}

// cpuFor returns the CPU chosen for a queue, or -1 if this queue has none.
func (p *affinityPlan) cpuFor(queue int) int {
	if p == nil || queue < 0 || queue >= len(p.cpu) {
		return -1
	}
	return p.cpu[queue]
}

// record notes that a queue's worker really was pinned, for Info and tests.
func (p *affinityPlan) record(queue, cpu int) {
	p.mu.Lock()
	p.pinned[queue] = cpu
	p.mu.Unlock()
}

// forget drops a queue's pinning record, for Socket.Unpin.
func (p *affinityPlan) forget(queue int) {
	p.mu.Lock()
	delete(p.pinned, queue)
	p.mu.Unlock()
}

// release puts back every interrupt this plan took, and only those.
func (p *affinityPlan) release() {
	if p == nil {
		return
	}
	for _, irq := range p.held {
		restoreIRQAffinity(irq)
	}
	p.held = nil
}

// String renders the plan for Fleet.Info, so what we did to the host — and to
// the caller's goroutines — is visible in the line applications already log at
// startup.
func (p *affinityPlan) String() string {
	if p == nil {
		return "off"
	}
	p.mu.Lock()
	pinned := len(p.pinned)
	p.mu.Unlock()

	planned := 0
	for _, cpu := range p.cpu {
		if cpu >= 0 {
			planned++
		}
	}
	if planned == 0 {
		if p.note != "" {
			return "off, " + p.note
		}
		return "off"
	}

	var b strings.Builder
	if pinned == 0 {
		// Info is usually read at startup, before any worker has reached the
		// datapath. Saying "0 pinned" then would read as a failure rather than
		// as not having happened yet.
		fmt.Fprintf(&b, "%d queues placed, pinned on first use", planned)
	} else {
		fmt.Fprintf(&b, "%d/%d workers pinned", pinned, planned)
	}
	if n := len(p.held); n > 0 {
		fmt.Fprintf(&b, ", %d interrupts moved", n)
	}
	if p.note != "" {
		fmt.Fprintf(&b, " (%s)", p.note)
	}
	return b.String()
}

// pin locks the calling goroutine to its thread and moves that thread to the
// CPU chosen for the queue. It reports the CPU, or -1 if it did not pin.
//
// The thread stays locked for the life of the goroutine: a worker that could be
// migrated is a worker whose next packet is on the wrong core, which is the
// whole thing we are trying to avoid.
func (p *affinityPlan) pin(queue int) (int, error) {
	cpu := p.cpuFor(queue)
	if cpu < 0 {
		return -1, nil
	}

	// Lock first: until the goroutine is wired to a thread, asking what that
	// thread's affinity is would be asking about a thread it may not keep.
	lockOSThread()

	// Is this thread already somebody's worker? Ask the kernel rather than
	// keep a table of thread ids. A locked goroutine that exits takes its
	// thread with it and Linux reuses the id, so a table keyed that way
	// eventually tells a brand new worker it is already pinned and silently
	// stops placing queues. A thread already confined to exactly one CPU is
	// the same answer without the bookkeeping.
	var cur unix.CPUSet
	if err := unix.SchedGetaffinity(0, &cur); err == nil && cur.Count() == 1 {
		// Driving two queues from one goroutine is a legitimate pattern; the
		// first assignment wins rather than dragging the thread back and
		// forth. It is also how a caller's own pinning is respected.
		if cur.IsSet(cpu) {
			p.record(queue, cpu) // already exactly where we wanted it
			return cpu, nil
		}
		unlockOSThread()
		return -1, nil
	}

	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		// Most likely the process is already confined (taskset, cpuset cgroup,
		// a container) and cpu is outside what it is allowed. That is the
		// operator being more specific than us, so respect it and carry on.
		unlockOSThread()
		return -1, fmt.Errorf("afxdp: pin queue %d to cpu %d: %w", queue, cpu, err)
	}
	p.record(queue, cpu)
	return cpu, nil
}

// queueIRQs maps an interface's queue indices to the interrupt numbers serving
// them.
//
// The device's own MSI-X vectors are listed in sysfs, which is what makes this
// safe on a dual-port card: both ports of a ConnectX are one driver with one
// interrupt naming scheme, and only the PCI function tells them apart. Each
// interrupt's handler name then appears as a subdirectory of /proc/irq/<n>,
// e.g. mlx5_comp7@pci:0000:c1:00.1 or eth0-TxRx-7, and the number in it is the
// queue.
func queueIRQs(iface string) (map[int][]int, error) {
	dir := filepath.Join("/sys/class/net", iface, "device", "msi_irqs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[int][]int)
	for _, e := range entries {
		irq, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		for _, name := range irqActions(irq) {
			if q, ok := queueFromIRQName(name); ok {
				out[q] = append(out[q], irq)
				break
			}
		}
	}
	for q := range out {
		sort.Ints(out[q])
	}
	return out, nil
}

// irqActions returns the handler names registered on an interrupt. The kernel
// exposes each as a directory under /proc/irq/<n>, alongside a fixed set of
// attribute files that are filtered out here.
func irqActions(irq int) []string {
	entries, err := os.ReadDir(filepath.Join("/proc/irq", strconv.Itoa(irq)))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// Interrupt handler names, by driver family. The queue index is the number in
// them; everything else about the name only says which driver wrote it.
//
//	mlx5_comp7@pci:0000:c1:00.1   mlx5
//	eth0-TxRx-7                   ixgbe, i40e, bnxt
//	eth0-Tx-Rx-7                  ena
//	eth0-rx-7 / eth0-tx-7         igb, and split-queue drivers generally
//	virtio0-input.7               virtio-net
//	eth0-7                        mlx4
var irqQueuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`_comp(\d+)@`),
	regexp.MustCompile(`-(?:input|output)\.(\d+)$`),
	regexp.MustCompile(`(?i)-(?:tx-?rx|rx-?tx|rx|tx|q|queue)-(\d+)$`),
	regexp.MustCompile(`-(\d+)$`),
}

// queueFromIRQName extracts the queue index from an interrupt handler name.
func queueFromIRQName(name string) (int, bool) {
	// Control interrupts share the naming scheme but serve no queue, and their
	// trailing number is a unit index, not a queue: pinning a worker to the
	// core handling link events would be worse than not pinning it at all.
	lower := strings.ToLower(name)
	for _, skip := range []string{"async", "mgmnt", "-event", "cmd", "page", "-misc", "-other", "config"} {
		if strings.Contains(lower, skip) {
			return 0, false
		}
	}
	for _, re := range irqQueuePatterns {
		if m := re.FindStringSubmatch(name); m != nil {
			q, err := strconv.Atoi(m[1])
			if err == nil && q >= 0 {
				return q, true
			}
		}
	}
	return 0, false
}

// irqSaved remembers the affinity an interrupt had before we touched it, so
// Close can put it back. Interrupts are host state shared by every process on
// the box, and more than one Fleet can be steering the same NIC, so this is
// refcounted: the first writer saves and the last releaser restores. Without
// the refcount a second Fleet would save our setting as if it were the
// original, and closing out of order would leave the box steered forever.
var (
	irqSavedMu sync.Mutex
	irqSaved   = make(map[int]*irqRef)
)

type irqRef struct {
	saved string
	n     int
}

func irqAffinityPath(irq int) string {
	return filepath.Join("/proc/irq", strconv.Itoa(irq), "smp_affinity_list")
}

// setIRQAffinity routes an interrupt to a single CPU, saving what was there.
// It reports whether it actually wrote anything.
func setIRQAffinity(irq, cpu int) bool {
	irqSavedMu.Lock()
	defer irqSavedMu.Unlock()

	if ref, ok := irqSaved[irq]; ok {
		ref.n++
		return writeFile(irqAffinityPath(irq), strconv.Itoa(cpu))
	}
	b, err := os.ReadFile(irqAffinityPath(irq))
	if err != nil {
		return false
	}
	if !writeFile(irqAffinityPath(irq), strconv.Itoa(cpu)) {
		return false
	}
	irqSaved[irq] = &irqRef{saved: strings.TrimSpace(string(b)), n: 1}
	return true
}

// restoreIRQAffinity puts an interrupt back once the last Fleet using it has
// closed. Interrupts we never wrote are not in the map and are left alone.
func restoreIRQAffinity(irq int) {
	irqSavedMu.Lock()
	defer irqSavedMu.Unlock()

	ref, ok := irqSaved[irq]
	if !ok {
		return
	}
	if ref.n--; ref.n > 0 {
		return
	}
	delete(irqSaved, irq)
	writeFile(irqAffinityPath(irq), ref.saved)
}

func writeFile(path, value string) bool {
	return os.WriteFile(path, []byte(value), 0o644) == nil
}

// singleCPU reports the CPU shared by every one of these interrupts, if they
// are each routed to exactly one CPU and it is the same one. A mask with
// several CPUs in it means the kernel is free to deliver anywhere, so there is
// no core for a worker to sit next to.
func singleCPU(irqs []int) (int, bool) {
	cpu := -1
	for _, irq := range irqs {
		b, err := os.ReadFile(irqAffinityPath(irq))
		if err != nil {
			return 0, false
		}
		list := parseCPUList(strings.TrimSpace(string(b)))
		if len(list) != 1 {
			return 0, false
		}
		if cpu >= 0 && list[0] != cpu {
			return 0, false
		}
		cpu = list[0]
	}
	if cpu < 0 {
		return 0, false
	}
	return cpu, true
}

func deviceNUMANode(iface string) int {
	b, err := os.ReadFile(filepath.Join("/sys/class/net", iface, "device", "numa_node"))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return n
}

func onlineCPUs() []int {
	if cpus := readCPUList("/sys/devices/system/cpu/online"); len(cpus) > 0 {
		return cpus
	}
	return nil
}

func readCPUList(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseCPUList(strings.TrimSpace(string(b)))
}

// parseCPUList parses the kernel's "0-3,8,12-15" CPU list format.
func parseCPUList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		start, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		end := start
		if isRange {
			if end, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if end < start || end-start > 4096 {
			continue
		}
		for c := start; c <= end; c++ {
			out = append(out, c)
		}
	}
	return out
}
