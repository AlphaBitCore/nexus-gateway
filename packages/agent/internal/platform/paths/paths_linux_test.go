//go:build linux

package paths

import (
	"strings"
	"testing"
)

// TestLinuxStatusSocketPath_XDGRuntimeDir verifies that when XDG_RUNTIME_DIR
// is set the socket path lands inside it.
func TestLinuxStatusSocketPath_XDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got := linuxStatusSocketPath()
	if !strings.HasPrefix(got, "/run/user/1000/") {
		t.Errorf("socket path = %q, want prefix /run/user/1000/", got)
	}
	if !strings.Contains(got, "nexus-agent") {
		t.Errorf("socket path = %q, want nexus-agent in path", got)
	}
}

// TestLinuxStatusSocketPath_NoXDGNoHome verifies that the last-resort fallback
// is /run/nexus-agent/ (not /tmp) when both XDG_RUNTIME_DIR and HOME are absent.
// F-0307: /tmp is world-accessible and predictable; /run/nexus-agent is created
// with mode 0700 and is only reachable by the daemon user.
func TestLinuxStatusSocketPath_NoXDGNoHome(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "")

	got := linuxStatusSocketPath()

	if strings.HasPrefix(got, "/tmp") {
		t.Errorf("socket path must NOT fall back to /tmp (world-accessible); got %q", got)
	}
	if !strings.HasPrefix(got, "/run/nexus-agent") {
		t.Errorf("socket path = %q, want /run/nexus-agent prefix for the privileged-daemon fallback", got)
	}
}
