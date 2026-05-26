//go:build linux && integration

package linux

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// startTCPEcho starts a localhost TCP listener that immediately
// closes any accepted connection — the integration tests don't need
// a real echo; they just need a connectable peer so the dialer
// makes a real socket whose options we can introspect.
func startTCPEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}

// readSOMARK reads the SO_MARK socket option from a TCP connection
// via getsockopt. Mirror of the agent's setsockopt path in
// linux_marker.go.
func readSOMARK(conn net.Conn) (uint32, error) {
	tcp := conn.(*net.TCPConn)
	raw, err := tcp.SyscallConn()
	if err != nil {
		return 0, err
	}
	var mark int
	var sysErr error
	err = raw.Control(func(fd uintptr) {
		mark, sysErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
	})
	if err != nil {
		return 0, err
	}
	if sysErr != nil {
		return 0, sysErr
	}
	return uint32(mark), nil
}
