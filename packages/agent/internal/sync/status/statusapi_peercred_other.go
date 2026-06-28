//go:build !darwin

package status

import "net"

// checkPeerUID is a no-op on non-macOS platforms. On Linux the socket is
// created with 0600 (owner-only), which already limits access to the daemon
// user without an in-process credential check. On Windows the status pipe's
// DACL grants the owner (the LocalSystem daemon) plus Interactive Users so the
// per-user Dashboard/tray can reach a SYSTEM-owned pipe (see
// statusapi_listen_windows.go); a real GetNamedPipeClientProcessId peer check
// to narrow that to the active console user is a follow-up, blocked by go-winio
// not exposing the client handle.
func checkPeerUID(_ net.Conn) error {
	return nil
}
