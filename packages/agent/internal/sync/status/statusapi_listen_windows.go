//go:build windows

package status

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

// platformListen creates a Windows named pipe listener with a restricted ACL.
func platformListen(path string) (net.Listener, error) {
	cfg := &winio.PipeConfig{
		// The daemon ships as a LocalSystem service (installer/NexusAgent.wxs
		// Account="LocalSystem"), so in production the pipe is owned by SYSTEM —
		// but the GUI Dashboard and tray run in the interactive user's session,
		// NOT as SYSTEM. An owner-only DACL ("D:P(A;;GA;;;OW)") therefore locked
		// the user's GUI out of a SYSTEM-owned pipe (the GUI could dial it but
		// not open it). Grant:
		//   (A;;GA;;;OW)   Owner (the daemon's account)  — full. Covers the
		//                  SYSTEM service AND a user-run daemon (dev / tests,
		//                  where the creator and the client are the same user).
		//   (A;;GRGW;;;IU) Interactive Users             — read/write. Lets the
		//                  per-user Dashboard/tray open the SYSTEM service's pipe.
		// IU narrows access to interactively-logged-on processes (excluding
		// service / network / batch logons). v1 ships single-user installs, so
		// the interactive user IS the legitimate user. Tighter multi-user
		// hardening — granting only the active console user's SID and/or a
		// GetNamedPipeClientProcessId peer check (the Windows analog of macOS
		// LOCAL_PEERCRED, see statusapi_peercred_other.go) — is a follow-up,
		// blocked today by go-winio v0.6.2 not exposing the client handle.
		SecurityDescriptor: "D:P(A;;GA;;;OW)(A;;GRGW;;;IU)",
	}
	ln, err := winio.ListenPipe(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("listen pipe %s: %w", path, err)
	}
	return ln, nil
}

// platformCleanup is a no-op on Windows — named pipes are cleaned up by the OS.
func platformCleanup(_ string) {}
