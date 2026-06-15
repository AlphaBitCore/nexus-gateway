//go:build darwin

package paths

// macOS path conventions follow Apple's File System Programming Guide:
// system-wide third-party state lives under /Library/Application Support/
// keyed by the app's reverse-DNS bundle identifier, and system-wide logs
// live under /Library/Logs/ in a dir of the same name. The LaunchDaemon
// plist is always /Library/LaunchDaemons/<bundle-id>.plist.
const bundleID = "com.nexus-gateway.agent"

func defaultPaths() Paths {
	stateDir := "/Library/Application Support/" + bundleID
	flagsDir := stateDir + "/flags"
	return Paths{
		StateDir:   stateDir,
		ConfigDir:  stateDir,
		ConfigFile: stateDir + "/agent.yaml",
		LogDir:     "/Library/Logs/" + bundleID,
		// System-wide path under /var/run/ so the root LaunchDaemon (write)
		// and any logged-in user's tray binary (connect) can both reach it.
		// The listen helper chmods the socket 0666 (world-connectable, the
		// cross-UID cost of root-daemon ↔ user-GUI on macOS). handleConn
		// enforces a LOCAL_PEERCRED UID check via GetsockoptXucred before
		// processing any command, so only a process running as the same UID
		// as the daemon can issue IPC requests (see statusapi_peercred_darwin.go).
		SocketPath:       "/var/run/nexus-agent-status.sock",
		FlagsDir:         flagsDir,
		UserQuitFlagPath: flagsDir + "/user-quit",
		DaemonUnitPath:   "/Library/LaunchDaemons/" + bundleID + ".plist",
	}
}
