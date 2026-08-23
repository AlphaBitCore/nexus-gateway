// Package local holds the CLI-only implementations that the shared agent kernel
// (nexus-agent-core/core) deliberately does not carry: the OS keychain
// SecretStore and the on-disk TOML profile store. Keeping them here means the
// web assistant — which embeds the kernel inside the Control Plane — pulls in
// neither go-keyring nor BurntSushi/toml. The kernel keeps the non-secret,
// transport-shaped Env type (its toml struct tags are inert without the lib);
// this package owns Config and its read/write/resolve behaviour.
package local

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local/paths"
)

// Config is the on-disk profile set at ~/.config/nexus/config.toml.
type Config struct {
	DefaultEnv string              `toml:"default_env"`
	Envs       map[string]core.Env `toml:"envs"`

	// LogLevel controls the diagnostic file logger's minimum level
	// (debug | info | warn | error). It defaults to "debug" when empty or
	// unrecognised so transport timings are captured out of the box — the log
	// is a file (never the TUI's stdout/stderr), so verbose by default costs
	// nothing the operator sees and everything when diagnosing a hang. See
	// SlogLevel.
	LogLevel string `toml:"log_level"`

	path string // source path, not serialized
}

// builtinLocalEnv is the out-of-the-box local target so a fresh clone can
// `nexus --env local login` without hand-writing a config first.
func builtinLocalEnv() core.Env {
	return core.Env{
		Name:             "local",
		CPBaseURL:        "http://localhost:3001",
		AIGatewayBaseURL: "http://localhost:3050",
		OAuthClientID:    "tui",
		OAuthRedirectURI: "http://localhost:3000/auth/callback",
		IsProd:           false,
	}
}

// DefaultConfigPath returns ~/.config/nexus/config.toml (OS-appropriate). It
// delegates to paths.DefaultPaths so the config and log locations have a single
// source of truth (the paths package), avoiding drift between where the config
// lives and where the startup banner says the log lives.
func DefaultConfigPath() (string, error) {
	p, err := paths.DefaultPaths()
	if err != nil {
		return "", err
	}
	return p.ConfigFile, nil
}

// SlogLevel maps the config's LogLevel string to an slog.Level. Unknown or empty
// values default to slog.LevelDebug: the operator wants full transport timings in
// the file log without having to opt in, and the file never touches the TUI's
// stdout/stderr, so the default is "log everything."
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelDebug
	}
}

// newDefaultConfig is the seed used when no config file exists yet.
func newDefaultConfig(path string) *Config {
	return &Config{
		DefaultEnv: "local",
		Envs:       map[string]core.Env{"local": builtinLocalEnv()},
		path:       path,
	}
}

// Load reads the config at path. A missing file is not an error: it returns the
// built-in defaults (local target, default_env=local) so first run works.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newDefaultConfig(path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Envs == nil {
		c.Envs = map[string]core.Env{}
	}
	c.path = path
	return &c, nil
}

// Save writes the config back to its source path, creating the directory with
// 0700 and the file with 0600. It never writes secrets — Env holds none.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// Follow a symlinked config to the file it points at. O_TRUNC wrote THROUGH
	// a symlink; rename would replace the link itself, so an operator whose
	// config is managed by stow / chezmoi / home-manager would silently lose the
	// link and every later edit to the real file. A path that does not exist yet
	// is the normal first-save case and stays as given.
	target := c.path
	if resolved, err := filepath.EvalSymlinks(c.path); err == nil {
		target = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("resolve config path: %w", err)
	}
	dir := filepath.Dir(target)
	// Write a sibling temp file and rename it over the target, rather than
	// opening the target with O_TRUNC. The truncate destroyed the existing
	// config BEFORE the replacement existed, so anything that went wrong after
	// it — a disk that filled, a close that failed, the process dying — left the
	// operator with no environments at all rather than the ones they had.
	// rename(2) within a directory is atomic, so a concurrent reader sees either
	// the old file or the new one and never a half-written one.
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		// Replacing atomically needs to create a sibling, so the DIRECTORY must
		// be writable — the old in-place write only needed the file to be. That
		// is a real behaviour change and deserves an actionable message rather
		// than a bare errno.
		return fmt.Errorf("create temp config in %s (the config directory must be writable "+
			"to replace the config atomically): %w", dir, err)
	}
	tmpName := tmp.Name()
	// Every path out of here except the successful rename must clean up. After
	// the rename the name is gone, so this is a no-op then.
	defer func() { _ = os.Remove(tmpName) }()

	// No explicit Chmod: os.CreateTemp already creates with 0600, and the mode is
	// pinned where it belongs — TestSaveLoad_RoundTrip asserts the saved file is
	// 0600. An extra syscall to restate it would only add an error path nothing
	// can reach.
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	// Sync before rename: rename orders the directory entry, not the file's
	// contents. Without this a crash just after the rename can leave the config
	// name pointing at an empty file — the exact outcome this change removes.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Resolve picks the active environment using the precedence
// flagEnv > sessionEnv > default_env. It returns a clear error when no
// name resolves or the named env is not defined.
func (c *Config) Resolve(flagEnv, sessionEnv string) (core.Env, error) {
	name := firstNonEmpty(flagEnv, sessionEnv, c.DefaultEnv)
	if name == "" {
		return core.Env{}, fmt.Errorf("no environment selected: pass --env, switch in-session, or set default_env")
	}
	env, ok := c.Envs[name]
	if !ok {
		return core.Env{}, fmt.Errorf("unknown environment %q (defined: %v)", name, c.envNames())
	}
	if env.Name == "" {
		env.Name = name
	}
	return env, nil
}

// SetEnv inserts or replaces a named environment.
func (c *Config) SetEnv(env core.Env) {
	if c.Envs == nil {
		c.Envs = map[string]core.Env{}
	}
	c.Envs[env.Name] = env
}

// RemoveEnv deletes a named environment. Removing the current default clears
// default_env (the operator picks a new one or it falls back to built-in local).
func (c *Config) RemoveEnv(name string) error {
	if _, ok := c.Envs[name]; !ok {
		return fmt.Errorf("unknown environment %q", name)
	}
	delete(c.Envs, name)
	if c.DefaultEnv == name {
		c.DefaultEnv = ""
	}
	return nil
}

// SetDefault points default_env at name, erroring if name is undefined.
func (c *Config) SetDefault(name string) error {
	if _, ok := c.Envs[name]; !ok {
		return fmt.Errorf("unknown environment %q", name)
	}
	c.DefaultEnv = name
	return nil
}

func (c *Config) envNames() []string {
	names := make([]string, 0, len(c.Envs))
	for n := range c.Envs {
		names = append(names, n)
	}
	return names
}

// firstNonEmpty returns the first non-empty argument. The kernel keeps its own
// copy (core/client.go uses it for IAM-action fallback); this is the local
// store's private copy so the two packages stay decoupled.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
