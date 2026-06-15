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
//     where both processes can reach it. World-connectable is the
//     required transport mode for cross-UID root↔user communication;
//     handleConn enforces a LOCAL_PEERCRED UID check (see
//     statusapi_peercred_darwin.go) before processing any command,
//     so only a process running as the same UID as the daemon can
//     issue IPC requests.
//
//   - Linux: 0600 (owner-only). The daemon and the desktop GUI run
//     as the same user, so the filesystem permission alone gates access.
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
