//go:build linux && !gvisorfork

package xdplink

import "gvisor.dev/gvisor/pkg/buffer"

// viewExternal: upstream gVisor has no external views, so nothing is ever
// transmitted in place.
func viewExternal(*buffer.View) (span []byte, read, write int, ok bool) { return nil, 0, 0, false }
