//go:build linux && !gvisorfork

package main

import "gvisor.dev/gvisor/pkg/tcpip/stack"

// applyForkFlags is a no-op without the gvisorfork build tag; the fork-only
// knobs live in fork_flags.go.
func applyForkFlags() {}

// forkTxHashFromRx reports whether connections transmit on their receive
// queue; upstream gVisor cannot.
func forkTxHashFromRx() bool { return false }

func forkOutOfOrder() uint64 { return 0 }

func forkDrainDeferred(*stack.Stack) {}

func forkLinkBackpressure() bool     { return false }
func forkResumeBlocked(*stack.Stack) {}

func forkProbeCounts() (tlp, rackLost uint64) { return 0, 0 }
