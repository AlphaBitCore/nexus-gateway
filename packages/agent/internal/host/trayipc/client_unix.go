//go:build !windows

package trayipc

import (
	"net"
	"time"
)

// dialPipe is only used on Windows. On Unix this stub keeps the
// compiler happy without dragging in winio.
func dialPipe(_ string, _ time.Duration) (net.Conn, error) {
	panic("trayipc: dialPipe called on a non-Windows build")
}
