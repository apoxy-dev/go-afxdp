//go:build linux

package afxdp

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestQueueFromIRQName(t *testing.T) {
	cases := []struct {
		name  string
		queue int
		ok    bool
	}{
		// One per driver family we expect to meet.
		{"mlx5_comp0@pci:0000:c1:00.1", 0, true},
		{"mlx5_comp23@pci:0000:c1:00.1", 23, true},
		{"eth0-TxRx-7", 7, true},
		{"eno2-Tx-Rx-3", 3, true},
		{"eth0-rx-2", 2, true},
		{"eth0-tx-2", 2, true},
		{"virtio0-input.5", 5, true},
		{"virtio0-output.5", 5, true},
		{"eth1-11", 11, true},
		{"ice-enp1s0-TxRx-9", 9, true},

		// Control interrupts share the naming scheme but serve no queue.
		// Pinning a worker next to the link-event handler would be worse than
		// not pinning it at all, so these must not match.
		{"mlx5_async0@pci:0000:c1:00.1", 0, false},
		{"ena-mgmnt@pci:0000:00:06.0", 0, false},
		{"i40e-eth0:misc", 0, false},
		{"eth0-event-0", 0, false},
		{"virtio0-config", 0, false},

		// Nothing that is not a NIC queue. queueIRQs only ever sees the
		// interrupts listed under the netdev's own PCI function, so these
		// cannot reach it in practice; the patterns stay narrow anyway.
		{"ahci0", 0, false},
		{"nvme0q1", 0, false},
		{"xhci_hcd", 0, false},
	}
	for _, c := range cases {
		q, ok := queueFromIRQName(c.name)
		if ok != c.ok || (ok && q != c.queue) {
			t.Errorf("queueFromIRQName(%q) = (%d, %v), want (%d, %v)", c.name, q, ok, c.queue, c.ok)
		}
	}
}

func TestParseCPUList(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"0", []int{0}},
		{"0-3", []int{0, 1, 2, 3}},
		{"0-3,8,12-13", []int{0, 1, 2, 3, 8, 12, 13}},
		{"", nil},
		{"garbage", nil},
		{"5-2", nil}, // reversed range yields nothing rather than looping
	}
	for _, c := range cases {
		if got := parseCPUList(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A plan for an interface with no per-queue interrupts must still be safe: no
// CPUs claimed, nothing to restore, and a String that says so.
func TestAffinityPlanNoIRQs(t *testing.T) {
	p := buildAffinityPlan("definitely-not-an-interface", 4, nil, false)
	for q, cpu := range p.cpu {
		if cpu != -1 {
			t.Errorf("queue %d got cpu %d, want none", q, cpu)
		}
	}
	if len(p.held) != 0 {
		t.Errorf("held %d irqs for a device with none", len(p.held))
	}
	if cpu, err := p.pin(0); cpu != -1 || err != nil {
		t.Errorf("pin on an unplanned queue = (%d, %v), want (-1, nil)", cpu, err)
	}
	p.release() // must not panic
}

// An explicit CPU list is followed even when no interrupts can be found, and
// wraps when there are more queues than CPUs.
func TestAffinityPlanExplicitCPUs(t *testing.T) {
	p := buildAffinityPlan("definitely-not-an-interface", 5, []int{2, 4}, false)
	want := []int{2, 4, 2, 4, 2}
	if !reflect.DeepEqual(p.cpu, want) {
		t.Errorf("cpu plan = %v, want %v", p.cpu, want)
	}
}

// pin must place the thread, must be idempotent per thread (a goroutine
// driving two queues pins once), and must leave the CPU it chose in the plan.
func TestAffinityPin(t *testing.T) {
	allowed := processCPUs(t)
	if len(allowed) < 2 {
		t.Skip("need two CPUs in this process's affinity mask")
	}
	// Two queues wanting *different* CPUs is the case that matters: one
	// goroutine cannot be in two places, so the first assignment must win and
	// the second must be reported as unpinned rather than silently dragging
	// the thread across.
	cpu, other := allowed[0], allowed[1]

	p := &affinityPlan{
		iface:  "test",
		cpu:    []int{cpu, other},
		irqs:   make([][]int, 2),
		pinned: make(map[int]int),
	}

	type result struct {
		first, second int
		mask          []int
	}
	done := make(chan result, 1)
	go func() {
		// The goroutine exits at the end of the test, which retires the locked
		// thread with it, so the rest of the run is unaffected.
		var r result
		r.first, _ = p.pin(0)
		r.second, _ = p.pin(1) // same goroutine, another queue's CPU: must not move
		var set unix.CPUSet
		if err := unix.SchedGetaffinity(0, &set); err == nil {
			for c := 0; c < len(allowed)+64; c++ {
				if set.IsSet(c) {
					r.mask = append(r.mask, c)
				}
			}
		}
		done <- r
		// Deliberately NOT runtime.UnlockOSThread: this thread is confined to
		// one CPU, and unlocking would hand it back to the runtime's pool with
		// that confinement still on it, quietly pinning whatever goroutine
		// lands on it next for the rest of the test binary. Letting the
		// goroutine exit while still locked retires the thread instead.
	}()
	r := <-done

	if r.first != cpu {
		t.Fatalf("pin(0) = %d, want %d", r.first, cpu)
	}
	if r.second != -1 {
		t.Errorf("pin(1) on an already-pinned thread = %d, want -1", r.second)
	}
	if !reflect.DeepEqual(r.mask, []int{cpu}) {
		t.Errorf("thread affinity after pin = %v, want [%d]", r.mask, cpu)
	}
	if got := p.pinned[0]; got != cpu {
		t.Errorf("plan recorded cpu %d for queue 0, want %d", got, cpu)
	}
	if _, ok := p.pinned[1]; ok {
		t.Errorf("queue 1 should not be recorded as pinned: %v", p.pinned)
	}
}

// The planner must never seat a worker on a core its own interrupt is using,
// must never hand the same physical core to two queues, and must keep the two
// halves of a queue in one cache complex whenever there is room.
func TestTopologyPlacement(t *testing.T) {
	topo := readTopology("definitely-not-an-interface")
	if len(topo.complexes) == 0 {
		t.Skip("cannot read CPU topology")
	}
	cores := 0
	for _, c := range topo.complexes {
		cores += len(c)
	}
	t.Logf("%d physical cores in %d cache complexes", cores, len(topo.complexes))

	seen := make(map[int]bool)
	pairs := 0
	for {
		a, b, ok := topo.freePairInAComplex()
		if !ok {
			break
		}
		if a == b {
			t.Fatalf("pair %d,%d is one core twice", a, b)
		}
		if topo.complexOf[a] != topo.complexOf[b] {
			t.Errorf("pair %d,%d spans two cache complexes", a, b)
		}
		for _, c := range []int{a, b} {
			if seen[c] {
				t.Errorf("core %d handed out twice", c)
			}
			seen[c] = true
		}
		topo.take(a)
		topo.take(b)
		pairs++
	}
	if pairs == 0 && cores >= 2 {
		t.Errorf("no pairs allocated from %d cores", cores)
	}
	if pairs > cores/2 {
		t.Errorf("allocated %d pairs from %d cores", pairs, cores)
	}
	// Taking a logical CPU must reserve the whole physical core, so a second
	// queue cannot land on its SMT sibling.
	topo2 := readTopology("definitely-not-an-interface")
	lead := topo2.complexes[0][0]
	sibs := topo2.siblings[lead]
	topo2.take(sibs[len(sibs)-1])
	if topo2.coreFree(lead) {
		t.Errorf("core %d still free after taking its sibling %d", lead, sibs[len(sibs)-1])
	}
}

// On a machine with a real NIC, the queue-to-interrupt map must cover the
// queues the interface actually has, and each interrupt must belong to that
// interface's own PCI function rather than a sibling port's.
func TestQueueIRQsOnRealNIC(t *testing.T) {
	ifaces, err := os.ReadDir("/sys/class/net")
	if err != nil {
		t.Skip(err)
	}
	for _, e := range ifaces {
		byQueue, err := queueIRQs(e.Name())
		if err != nil || len(byQueue) == 0 {
			continue
		}
		n, err := CountQueues(e.Name())
		if err != nil {
			continue
		}
		t.Logf("%s: %d rx queues, %d with interrupts", e.Name(), n, len(byQueue))
		seen := make(map[int]int)
		for q, irqs := range byQueue {
			for _, irq := range irqs {
				if prev, dup := seen[irq]; dup {
					t.Errorf("%s: irq %d claimed by queues %d and %d", e.Name(), irq, prev, q)
				}
				seen[irq] = q
			}
		}
		return
	}
	t.Skip("no interface with per-queue interrupts on this machine")
}

func processCPUs(t *testing.T) []int {
	t.Helper()
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		t.Skip(err)
	}
	var out []int
	for c := 0; c < 4096; c++ {
		if set.IsSet(c) {
			out = append(out, c)
		}
	}
	return out
}

// synthetic builds a machine of the given shape without touching sysfs:
// complexes cache complexes, coresPer physical cores in each, and threads
// logical CPUs on every core. CPU numbering matches the usual Linux layout,
// where every core's first thread is numbered before any core's second.
func synthetic(complexes, coresPer, threads int) *topology {
	t := &topology{
		siblings:  make(map[int][]int),
		leadOf:    make(map[int]int),
		complexOf: make(map[int]int),
		taken:     make(map[int]bool),
	}
	physical := complexes * coresPer
	for cx := range complexes {
		var cores []int
		for i := range coresPer {
			lead := cx*coresPer + i
			sibs := make([]int, threads)
			for th := range threads {
				cpu := lead + th*physical
				sibs[th] = cpu
				t.leadOf[cpu] = lead
			}
			t.siblings[lead] = sibs
			t.complexOf[lead] = cx
			cores = append(cores, lead)
		}
		t.complexes = append(t.complexes, cores)
	}
	return t
}

// place runs the same three-choice cascade buildAffinityPlan uses, against a
// synthetic machine. Keeping it in step with the real loop is the point: the
// invariants below are what the placement is *for*, and until this existed the
// cascade had no test at all.
func place(topo *topology, irqCPU []int, nQueues int) (worker, irq []int) {
	worker = make([]int, nQueues)
	irq = make([]int, nQueues)
	for q := range worker {
		worker[q], irq[q] = -1, -1
	}
	for q := range nQueues {
		if irqCPU[q] >= 0 {
			topo.take(irqCPU[q])
		}
	}
	for q := range nQueues {
		if cur := irqCPU[q]; cur >= 0 {
			topo.release(cur)
			if w, ok := topo.freeInComplexOf(cur); ok {
				irq[q], worker[q] = cur, w
				topo.take(cur)
				topo.take(w)
				continue
			}
			topo.take(cur)
		}
		if a, b, ok := topo.freePairInAComplex(); ok {
			if cur := irqCPU[q]; cur >= 0 {
				topo.release(cur)
			}
			irq[q], worker[q] = a, b
			topo.take(a)
			topo.take(b)
			continue
		}
		if a, sib, ok := topo.freeCoreWithSibling(); ok {
			if cur := irqCPU[q]; cur >= 0 {
				topo.release(cur)
			}
			irq[q], worker[q] = a, sib
			topo.take(a)
			continue
		}
	}
	return worker, irq
}

// TestPlacementInvariants is the safety net for the placement cascade. The
// property that matters is not which core a queue gets but that no worker ever
// lands on a physical core that is serving a bound queue's interrupt: that is
// the arrangement measured at 0.99 Mpps against 30.5, and it is what a queue is
// left unplaced rather than given.
func TestPlacementInvariants(t *testing.T) {
	shapes := []struct {
		name                         string
		complexes, coresPer, threads int
	}{
		{"epyc-like 8x3 SMT", 8, 3, 2},
		{"one complex, SMT", 1, 6, 2},
		{"one complex, no SMT", 1, 8, 1},
		{"many small complexes", 12, 2, 2},
		{"single core", 1, 1, 1},
	}
	for _, sh := range shapes {
		physical := sh.complexes * sh.coresPer
		for _, nQueues := range []int{1, 2, physical / 2, physical, physical * 2} {
			if nQueues < 1 {
				continue
			}
			t.Run(fmt.Sprintf("%s/%dq", sh.name, nQueues), func(t *testing.T) {
				topo := synthetic(sh.complexes, sh.coresPer, sh.threads)
				// Driver hints: interrupt for queue q on CPU q, as mlx5 and
				// friends leave them.
				irqCPU := make([]int, nQueues)
				for q := range irqCPU {
					irqCPU[q] = q % (physical * sh.threads)
				}
				worker, irq := place(topo, irqCPU, nQueues)

				live := make(map[int]int) // physical core -> queue whose IRQ is on it
				for q := range nQueues {
					c := irq[q]
					if c < 0 {
						c = irqCPU[q] // not moved: still where the driver put it
					}
					if lead, ok := topo.leadOf[c]; ok {
						live[lead] = q
					}
				}
				seen := make(map[int]int)
				placed := 0
				for q := range nQueues {
					w := worker[q]
					if w < 0 {
						continue
					}
					placed++
					lead, ok := topo.leadOf[w]
					if !ok {
						t.Fatalf("queue %d: worker on unknown cpu %d", q, w)
					}
					// THE invariant: never on a core serving *another* queue's
					// interrupt. Sharing a core with this queue's own interrupt
					// is the documented third choice — the worker takes the SMT
					// sibling — and costs a receiver about a third of its
					// packets, where landing on someone else's live interrupt
					// costs it almost everything.
					if other, clash := live[lead]; clash && other != q {
						t.Errorf("queue %d worker on cpu %d (core %d), which serves queue %d's interrupt",
							q, w, lead, other)
					}
					if prev, dup := seen[lead]; dup {
						t.Errorf("core %d given to queues %d and %d", lead, prev, q)
					}
					seen[lead] = q
					// If the worker shares a core with its own interrupt, it
					// must be on a different logical CPU — sharing the CPU
					// itself is the arrangement that collapses a receiver.
					if irq[q] >= 0 && w == irq[q] {
						t.Errorf("queue %d: worker and interrupt on the same cpu %d", q, w)
					}
					// A placed queue's two halves should share a cache complex.
					if ic := irq[q]; ic >= 0 {
						if topo.complexOf[topo.leadOf[ic]] != topo.complexOf[lead] {
							t.Errorf("queue %d: worker cpu %d and interrupt cpu %d are in different complexes", q, w, ic)
						}
					}
				}
				// With room to spare, everything should be placed.
				if nQueues <= physical/2 && placed != nQueues {
					t.Errorf("placed %d of %d queues with %d cores available", placed, nQueues, physical)
				}
			})
		}
	}
}
