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
//  3. /tmp/nexus-agent-status.sock — last-resort if the home dir is
//     unreadable; world-readable and not great for security, but better
//     than crashing.
func linuxStatusSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "nexus-agent-status.sock")
	}
	if home, err := os.UserHomeDir(); err == nil {
		nexusDir := filepath.Join(home, ".nexus")
		_ = os.MkdirAll(nexusDir, 0700)
		return filepath.Join(nexusDir, "agent-status.sock")
	}
	return "/tmp/nexus-agent-status.sock"
}
