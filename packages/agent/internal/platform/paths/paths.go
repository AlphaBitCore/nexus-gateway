package paths

// Paths returns the OS-idiomatic locations the agent reads from and
// writes to. Each platform implementation lives in paths_<goos>.go.
//
// Used by:
//   - config.AgentConfig: fills CertFile/KeyFile/CACertFile/HubCACertFile/
//     AuditDBPath/Log.File when the YAML leaves them blank, so configs only
//     need to override exceptions.
//   - enrollment manager bootstrap: enroll writes certs into StateDir so the
//     daemon picks them up without a manual copy step.
//   - installer scripts: pkg layout / launchd plist / uninstaller targets.
//
// Path conventions per OS:
//
//	darwin  → /Library/Application Support/com.nexus-gateway.agent/
//	          /Library/Logs/com.nexus-gateway.agent/
//	          (Apple File System Programming Guide)
//	linux   → /var/lib/nexus-agent/, /etc/nexus-agent/, /var/log/nexus-agent/
//	          (FHS 3.0)
//	windows → %ProgramData%\NexusAgent\
//	          (Windows Application Data conventions)
type Paths struct {
	// StateDir is where the agent persists runtime state: device cert + key,
	// the gateway CA, device-id / thing-id / device-token, and the
	// SQLCipher-encrypted audit queue.
	StateDir string

	// ConfigDir is where agent.yaml lives. On macOS this is the same as
	// StateDir (Apple's convention is one bundle dir per app); on Linux
	// it's /etc/nexus-agent so it follows FHS.
	ConfigDir string

	// ConfigFile is the absolute path of the agent.yaml file the daemon
	// reads by default (ConfigDir + "agent.yaml").
	ConfigFile string

	// LogDir holds the structured slog json file. LaunchDaemon stdout/stderr
	// go here too on macOS.
	LogDir string

	// SocketPath is the IPC socket path the daemon listens on for status
	// queries from the menu-bar UI.
	SocketPath string

	// FlagsDir holds user-space-writable signal files the GUI app sets to
	// communicate intent to the root-owned daemon. Currently:
	//   user-quit  — presence tells the daemon to self-exit (and to
	//                self-exit again on every launchd respawn) until the
	//                GUI clears the flag on its next launch. See
	//                [[agent-quit-flag-design]].
	// The directory itself is created and chmod-ed to 0777 by the
	// installer's post-install hook (mode 0777 with no sticky bit so the
	// GUI app can both create and delete files inside it); the daemon
	// owns the parent StateDir.
	FlagsDir string
	// UserQuitFlagPath is FlagsDir + "/user-quit"; resolved by
	// DefaultPaths so the daemon's flag-watcher and the GUI app's
	// QuitFlag helper agree on the path without copy-pasting it.
	UserQuitFlagPath string

	// DaemonUnitPath is where the OS process supervisor expects the
	// daemon's launch configuration. macOS launchd plist, Linux systemd
	// unit, Windows service entry.
	DaemonUnitPath string
}

// DefaultPaths returns the canonical paths for the current OS.
// Implementations live in paths_darwin.go / paths_linux.go / paths_windows.go.
var DefaultPaths = defaultPaths
