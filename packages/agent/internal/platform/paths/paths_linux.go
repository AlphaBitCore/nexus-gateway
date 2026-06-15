//go:build linux

package paths

import (
	"os"
	"path/filepath"
)

// Linux paths follow the Filesystem Hierarchy Standard (FHS 3.0):
//
//	/var/lib/<pkg>     persistent state
//	/etc/<pkg>         system-wide config
//	/var/log/<pkg>     log files
//	$XDG_RUNTIME_DIR or ~/.nexus/  runtime IPC socket (per-user)
//	/etc/systemd/system/<pkg>.service  service unit
const pkgName = "nexus-agent"

func defaultPaths() Paths {
	stateDir := "/var/lib/" + pkgName
	flagsDir := stateDir + "/flags"
	return Paths{
		StateDir:         stateDir,
		ConfigDir:        "/etc/" + pkgName,
		ConfigFile:       "/etc/" + pkgName + "/agent.yaml",
		LogDir:           "/var/log/" + pkgName,
		SocketPath:       linuxStatusSocketPath(),
		FlagsDir:         flagsDir,
		UserQuitFlagPath: flagsDir + "/user-quit",
		DaemonUnitPath:   "/etc/systemd/system/" + pkgName + ".service",
	}
}

// linuxStatusSocketPath returns the IPC socket path that the daemon and the
// tray UI both consume. v1 ships single-user installs: the daemon and the
// tray run as the SAME logged-in user, so a per-user runtime dir is fine.
//
// Order of preference:
//  1. $XDG_RUNTIME_DIR/nexus-agent-status.sock — systemd-logind-provisioned
//     per-user runtime dir; the standard place for per-user sockets.
//  2. ~/.nexus/agent-status.sock — fallback when XDG_RUNTIME_DIR is empty;
//     also creates ~/.nexus with 0700.
//  3. /run/nexus-agent/agent-status.sock — privileged-daemon fallback when
//     both XDG_RUNTIME_DIR and HOME are absent (e.g. root daemon with stripped
//     env). The directory is created with 0700 so the socket is inaccessible
//     to other users. This replaces the former /tmp fallback which used a
//     world-accessible, predictable path.
func linuxStatusSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "nexus-agent-status.sock")
	}
	if home, err := os.UserHomeDir(); err == nil {
		nexusDir := filepath.Join(home, ".nexus")
		_ = os.MkdirAll(nexusDir, 0700)
		return filepath.Join(nexusDir, "agent-status.sock")
	}
	// Privileged daemon with no user home — use /run/nexus-agent/ (mode 0700)
	// instead of /tmp (world-accessible and predictable). Creating the directory
	// here is best-effort; if it fails the daemon will fail to bind the socket
	// on its own rather than silently using an insecure path.
	runDir := "/run/nexus-agent"
	_ = os.MkdirAll(runDir, 0700)
	return filepath.Join(runDir, "agent-status.sock")
}
