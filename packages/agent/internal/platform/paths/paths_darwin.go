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
		// The daemon sets mode 0666 on the socket file; IPC handlers
		// enforce origin checks before performing privileged operations.
		SocketPath:       "/var/run/nexus-agent-status.sock",
		FlagsDir:         flagsDir,
		UserQuitFlagPath: flagsDir + "/user-quit",
		DaemonUnitPath:   "/Library/LaunchDaemons/" + bundleID + ".plist",
	}
}
