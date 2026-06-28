//go:build windows

package main

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialStatus connects to the daemon's statusapi named pipe. The daemon's
// statusapi platformListen (statusapi_listen_windows.go) creates the pipe
// with an owner-only DACL, so only the current user's processes can
// connect. Mirrors trayipc/client_windows.go::dialPipe so the Dashboard
// and tray dial the pipe identically.
func dialStatus(path string, timeout time.Duration) (net.Conn, error) {
	to := timeout
	return winio.DialPipe(path, &to)
}
