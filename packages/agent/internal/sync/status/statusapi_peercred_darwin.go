//go:build darwin

package status

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// noConsoleUID marks "no GUI session owner could be determined" — used when
// /dev/console cannot be stat'd. It is deliberately a value no real account
// holds so peerUIDAllowed can never match a peer against it.
const noConsoleUID = -1

// checkPeerUID gates the world-connectable macOS status socket. The daemon
// runs as root while its only legitimate clients — the menu-bar app and the
// Wails Dashboard — run in the console user's GUI session, so the socket is
// chmod 0666 and reachable across UIDs. LOCAL_PEERCRED gives us the connecting
// process's real UID; peerUIDAllowed then admits only root (the daemon itself
// and admin tooling) and the current console user, rejecting any other local
// account that can reach the socket on a multi-user host.
//
// Returning a non-nil error causes handleConn to close the connection before
// processing any commands.
func checkPeerUID(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		// Not a Unix socket (e.g. in tests using net.Pipe). Skip the check —
		// the test harness is trusted.
		return nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("peercred: SyscallConn: %w", err)
	}

	var cred *unix.Xucred
	var sockErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if ctrlErr != nil {
		return fmt.Errorf("peercred: Control: %w", ctrlErr)
	}
	if sockErr != nil {
		return fmt.Errorf("peercred: GetsockoptXucred: %w", sockErr)
	}

	peerUID := int(cred.Uid)
	selfUID := os.Getuid()
	consoleUID := consoleOwnerUID()
	if !peerUIDAllowed(peerUID, selfUID, consoleUID) {
		return fmt.Errorf("peercred: peer UID %d is neither the daemon UID %d nor the console user %d: connection rejected",
			peerUID, selfUID, consoleUID)
	}
	return nil
}

// peerUIDAllowed reports whether a connecting peer may issue IPC commands.
// Allowed peers are the daemon's own UID (root, or the same user in a
// single-user install) and the current console user (the GUI session that
// owns the menu-bar app and Dashboard). A consoleUID of noConsoleUID never
// matches, so when no GUI session exists only the daemon UID is admitted.
func peerUIDAllowed(peerUID, selfUID, consoleUID int) bool {
	if peerUID == selfUID {
		return true
	}
	if consoleUID != noConsoleUID && peerUID == consoleUID {
		return true
	}
	return false
}

// consoleOwnerUID returns the UID that owns /dev/console — the kernel's record
// of which user holds the active GUI session. Returns noConsoleUID when the
// device node cannot be stat'd (no usable console owner), which keeps the
// socket closed to non-root peers rather than guessing.
func consoleOwnerUID() int {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return noConsoleUID
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return noConsoleUID
	}
	return int(st.Uid)
}
