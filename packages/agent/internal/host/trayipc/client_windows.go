//go:build windows

package trayipc

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialPipe connects to a Windows named pipe with the given timeout.
// The daemon's statusapi platformListen creates the pipe with
// owner-only DACL so only the current user's processes can connect.
func dialPipe(path string, timeout time.Duration) (net.Conn, error) {
	to := timeout
	return winio.DialPipe(path, &to)
}
