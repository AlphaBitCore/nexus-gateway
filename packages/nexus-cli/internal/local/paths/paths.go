// Package paths resolves the OS-idiomatic, USER-scoped locations the nexus-cli
// reads from and writes to. It mirrors the structure of the agent's build-tagged
// paths package (packages/agent/internal/platform/paths) — one platform file per
// GOOS — but every location is user-scoped, not system-wide: the agent is a root
// daemon writing to /Library/Application Support, /var/lib, %ProgramData%; the CLI
// is a per-user app, so its config and logs live under the invoking user's home.
//
// Used by:
//   - local.DefaultConfigPath: the single source of truth for the config.toml
//     location (delegates here so config + log paths agree).
//   - local.OpenLogger: the file logger creates LogDir and appends to LogFile.
//   - cli.App: the startup banner logs ConfigFile + LogFile so an operator can
//     find the log from a "where are my logs?" question.
//
// Path conventions per OS (config keeps os.UserConfigDir(); logs follow each
// platform's user-scoped log/state convention):
//
//	darwin  → config: $HOME/Library/Application Support/nexus/config.toml
//	          logs:   $HOME/Library/Logs/nexus/nexus-cli.log
//	          (Apple File System Programming Guide — per-user Logs dir)
//	linux   → config: $XDG_CONFIG_HOME (or $HOME/.config)/nexus/config.toml
//	          logs:   $XDG_STATE_HOME (or $HOME/.local/state)/nexus/nexus-cli.log
//	          (XDG Base Directory Specification — state/logs live in XDG_STATE_HOME)
//	windows → config: %AppData%\nexus\config.toml
//	          logs:   %LocalAppData%\nexus\Logs\nexus-cli.log
//	          (Windows Application Data conventions — machine-local logs in LocalAppData)
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Paths are the resolved user-scoped locations for the current OS.
type Paths struct {
	// ConfigFile is the absolute path of the TOML profile the CLI reads
	// (os.UserConfigDir()/nexus/config.toml). Unchanged from the historical
	// location so existing configs keep working.
	ConfigFile string

	// LogDir is the user-scoped directory the CLI writes its diagnostic log
	// into. Created (0700) on first log open.
	LogDir string

	// LogFile is LogDir + "/nexus-cli.log" — the single append-mode slog text
	// file. The logger rotates it to LogFile+".1" when it grows past its cap.
	LogFile string
}

// userConfigDir and resolveLogDir are indirections over os.UserConfigDir and the
// build-tagged userLogDir so a test can force either resolution to fail
// independently (the two read overlapping env vars, so they cannot be separated
// by env manipulation alone). Production code never reassigns them.
var (
	userConfigDir = os.UserConfigDir
	resolveLogDir = userLogDir
)

// DefaultPaths resolves the canonical user-scoped paths for the current OS.
// ConfigFile uses os.UserConfigDir() (kept stable across platforms so the
// config location does not move); LogDir uses the per-OS userLogDir() defined
// in paths_<goos>.go.
func DefaultPaths() (Paths, error) {
	cfgDir, err := userConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve user config dir: %w", err)
	}
	logDir, err := resolveLogDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve user log dir: %w", err)
	}
	return Paths{
		ConfigFile: filepath.Join(cfgDir, "nexus", "config.toml"),
		LogDir:     logDir,
		LogFile:    filepath.Join(logDir, "nexus-cli.log"),
	}, nil
}
