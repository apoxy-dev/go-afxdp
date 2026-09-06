//go:build linux && !gvisorfork

package main

import "log"

func (c *rtcConn) serveFilesRTC([]byte) {
	log.Printf("-mode files-rtc needs the gVisor fork (build with -tags gvisorfork)")
	c.close()
}
