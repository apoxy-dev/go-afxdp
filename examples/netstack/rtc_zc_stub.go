//go:build linux && !gvisorfork

package main

import "log"

// serveSource needs the fork's WriteExternal; upstream gVisor copies every
// byte it sends.
func (c *rtcConn) serveSource() {
	log.Printf("-mode source-zc needs the gVisor fork (build with -tags gvisorfork)")
	c.close()
}
