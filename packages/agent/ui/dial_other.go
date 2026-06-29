//go:build !windows

package main

import (
	"net"
	"time"
)

// dialStatus connects to the daemon's statusapi Unix-domain socket. The
// daemon creates it with owner-only permissions, so only the current
// user's processes can connect. Mirrors trayipc.dialDeadline's non-Windows
// branch so the Dashboard and tray dial identically.
func dialStatus(path string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.Dial("unix", path)
}
