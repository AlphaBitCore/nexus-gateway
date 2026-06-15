//go:build !darwin

package status

import "net"

// checkPeerUID is a no-op on non-macOS platforms. On Linux the socket is
// created with 0600 (owner-only), which already limits access to the daemon
// user without an in-process credential check. On Windows the status pipe is
// created with a DACL restricting access to the service SID
// (see statusapi_listen_windows.go).
func checkPeerUID(_ net.Conn) error {
	return nil
}
