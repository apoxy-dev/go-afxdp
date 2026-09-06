//go:build linux && gvisorfork

package xdplink

import "gvisor.dev/gvisor/pkg/buffer"

// viewExternal reports a strided external view's memory and window (the
// fork's View.External).
func viewExternal(v *buffer.View) (span []byte, read, write int, ok bool) { return v.External() }
