//go:build windows

package status

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

// platformListen creates a Windows named pipe listener with restricted ACL.
func platformListen(path string) (net.Listener, error) {
	cfg := &winio.PipeConfig{
		// SDDL: allow only the creating user (owner) full access.
		SecurityDescriptor: "D:P(A;;GA;;;OW)",
	}
	ln, err := winio.ListenPipe(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("listen pipe %s: %w", path, err)
	}
	return ln, nil
}

// platformCleanup is a no-op on Windows — named pipes are cleaned up by the OS.
func platformCleanup(_ string) {}
