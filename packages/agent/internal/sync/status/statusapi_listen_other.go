//go:build !windows

package status

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
)

// platformListen creates the Unix domain socket listener.
//
// Socket mode is platform-specific:
//
//   - macOS: 0666. The daemon runs as a root LaunchDaemon and the
//     Wails Dashboard runs in the user's session — they MUST be able
//     to communicate across UIDs, and the socket lives in /var/run
//     where both processes can reach it. World-connectable is
//     acceptable for v1 single-user desktop installs because:
//     (a) the IPC protocol exposes only status queries +
//     enroll/pause operations, no shell/code-exec,
//     (b) any local user could DoS the daemon many other ways
//     (e.g. fill the audit DB volume).
//     Multi-user enterprise hardening (LOCAL_PEERCRED UID check or
//     group-based ACL) is tracked as future work.
//
//   - Linux: 0600 today (single-user dev/prod), tightened to 0660
//     with a nexus-agent group + the user added at install time
//     for cross-UID flows (daemon as nexus-agent, tray/dashboard
//     as the desktop user).
func platformListen(path string) (net.Listener, error) {
	_ = os.Remove(path) // remove stale socket
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	mode := os.FileMode(0600)
	if runtime.GOOS == "darwin" {
		mode = 0666
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket %s: %w", path, err)
	}
	return ln, nil
}

// platformCleanup removes the Unix socket file.
func platformCleanup(path string) {
	_ = os.Remove(path)
}
