//go:build linux

package afxdp

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
)

// countingProgram returns a ProgramFunc whose program adds 1 to counter for
// every packet, then drops it, or redirects it to the socket of its queue the
// way the built-in programs do.
func countingProgram(counter *ebpf.Map, drop bool) ProgramFunc {
	return func(sockets, queues *ebpf.Map) (*ebpf.Program, error) {
		ins := asm.Instructions{
			asm.Mov.Reg(asm.R6, asm.R1),
			asm.StoreImm(asm.RFP, -4, 0, asm.Word),
			asm.Mov.Reg(asm.R2, asm.RFP),
			asm.Add.Imm(asm.R2, -4),
			asm.LoadMapPtr(asm.R1, counter.FD()),
			asm.FnMapLookupElem.Call(),
			asm.JEq.Imm(asm.R0, 0, "verdict"),
			asm.Mov.Imm(asm.R1, 1),
			asm.StoreXAdd(asm.R0, asm.R1, asm.DWord),
		}
		if drop {
			ins = append(ins,
				asm.Mov.Imm(asm.R0, 1).WithSymbol("verdict"), // XDP_DROP
				asm.Return(),
			)
		} else {
			ins = append(ins,
				asm.LoadMem(asm.R2, asm.R6, 16, asm.Word).WithSymbol("verdict"), // rx_queue_index
				asm.StoreMem(asm.RFP, -8, asm.R2, asm.Word),
				asm.Mov.Reg(asm.R2, asm.RFP),
				asm.Add.Imm(asm.R2, -8),
				asm.LoadMapPtr(asm.R1, queues.FD()),
				asm.FnMapLookupElem.Call(),
				asm.JEq.Imm(asm.R0, 0, "pass"),
				asm.LoadMem(asm.R1, asm.R0, 0, asm.Word),
				asm.JEq.Imm(asm.R1, 0, "pass"),
				asm.LoadMapPtr(asm.R1, sockets.FD()),
				asm.LoadMem(asm.R2, asm.R6, 16, asm.Word),
				asm.Mov.Imm(asm.R3, 0),
				asm.FnRedirectMap.Call(),
				asm.Return(),
				asm.Mov.Imm(asm.R0, xdpPass).WithSymbol("pass"),
				asm.Return(),
			)
		}
		return ebpf.NewProgram(&ebpf.ProgramSpec{
			Name:         "caller_xdp",
			Type:         ebpf.XDP,
			Instructions: ins,
			License:      "LGPL-2.1 or BSD-2-Clause",
		})
	}
}

// TestWithProgram opens a receive fleet with the caller's program and checks
// that the program sees each packet and decides what reaches the socket.
func TestWithProgram(t *testing.T) {
	cases := []struct {
		name string
		drop bool
		want int // test frames that reach the socket
	}{
		{name: "redirect", drop: false, want: 100},
		{name: "drop", drop: true, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ifA, ifB := newVethPair(t)
			if err := rlimit.RemoveMemlock(); err != nil {
				t.Skipf("cannot raise memlock (need privileges): %v", err)
			}
			counter, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
			if err != nil {
				t.Skipf("cannot create a BPF map: %v", err)
			}
			defer counter.Close()
			tx, err := Open(ifA, WithGenericMode(), WithFilter(MatchNone()))
			if err != nil {
				t.Fatalf("open tx: %v", err)
			}
			defer tx.Close()
			rx, err := Open(ifB, WithGenericMode(), WithProgram(countingProgram(counter, tc.drop)))
			if err != nil {
				t.Fatalf("open rx: %v", err)
			}
			defer rx.Close()
			if info, err := rx.Info(); err != nil || info.Filter != "caller program" {
				t.Errorf("Info() = %+v, %v, want the filter %q", info, err, "caller program")
			}
			if !tx.WaitLinkUp(5 * time.Second) {
				t.Fatal("veth link did not come up")
			}

			const sent = 100
			batch := make([][]byte, sent)
			for i := range batch {
				batch[i] = testFrame()
			}
			if _, err := tx.Sockets()[0].SendBatch(batch); err != nil {
				t.Fatalf("SendBatch: %v", err)
			}
			wait := time.Now().Add(5 * time.Second)
			if tc.want == 0 {
				wait = time.Now().Add(time.Second)
			}
			if got := receiveCount(rx.Sockets()[0], sent, wait); got != tc.want {
				t.Errorf("received %d test frames, want %d", got, tc.want)
			}
			var n uint64
			if err := counter.Lookup(uint32(0), &n); err != nil {
				t.Fatalf("read counter: %v", err)
			}
			// A live veth also carries kernel traffic, so the program can see more.
			if n < sent {
				t.Errorf("program saw %d packets, want at least %d", n, sent)
			}
		})
	}
}

// TestWithProgramErrors checks that Open refuses a caller program it cannot
// use, and leaves nothing attached.
func TestWithProgramErrors(t *testing.T) {
	notXDP := func(*ebpf.Map, *ebpf.Map) (*ebpf.Program, error) {
		return ebpf.NewProgram(&ebpf.ProgramSpec{
			Type:         ebpf.SocketFilter,
			Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, 0), asm.Return()},
			License:      "LGPL-2.1 or BSD-2-Clause",
		})
	}
	cases := []struct {
		name string
		opts []Option
		want string // in the error
	}{
		{
			name: "with a filter",
			opts: []Option{WithProgram(countingProgram(nil, true)), WithFilter(MatchAll())},
			want: "WithProgram replaces the filter",
		},
		{
			name: "with management exceptions",
			opts: []Option{WithProgram(countingProgram(nil, true)), WithKeepManagement()},
			want: "WithProgram replaces the filter",
		},
		{
			name: "build error",
			opts: []Option{WithProgram(func(*ebpf.Map, *ebpf.Map) (*ebpf.Program, error) { return nil, errors.New("no object") })},
			want: "no object",
		},
		{
			name: "no program",
			opts: []Option{WithProgram(func(*ebpf.Map, *ebpf.Map) (*ebpf.Program, error) { return nil, nil })},
			want: "no program",
		},
		{
			name: "not an XDP program",
			opts: []Option{WithProgram(notXDP)},
			want: "not XDP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ifB := newVethPair(t)
			if err := rlimit.RemoveMemlock(); err != nil {
				t.Skipf("cannot raise memlock (need privileges): %v", err)
			}
			f, err := Open(ifB, append([]Option{WithGenericMode()}, tc.opts...)...)
			if err == nil {
				f.Close()
				t.Fatal("Open succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
			l, err := netlink.LinkByName(ifB)
			if err != nil {
				t.Fatalf("look up %s: %v", ifB, err)
			}
			if xdpAttached(l) {
				t.Error("an XDP program is attached after the failed Open")
			}
		})
	}
}
