// Package cli wires the `nexus` command tree (Cobra) over internal/core. Each
// command is a thin presenter: it calls a typed core capability and renders the
// result as a table (default) or stable JSON (--output json).
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local/paths"
)

// Version is the build version stamped into the session-start log line. It is
// "dev" for un-stamped local builds and overridden via -ldflags at release.
var Version = "dev"

// errUsage marks an argument/usage error so the exit-code mapper returns 2.
var errUsage = errors.New("usage error")

// App holds the shared state every command needs. Tests construct an App with
// Client/Out/Format preset to bypass config loading and real auth.
type App struct {
	Cfg        *local.Config
	Env        core.Env
	Store      core.SecretStore
	HTTP       *http.Client
	Out        io.Writer
	ErrOut     io.Writer
	Format     string // "table" | "json"
	EnvFlag    string // value of --env
	ConfigPath string // override for tests; empty → DefaultConfigPath
	SkillDir   string // override for tests; empty → capabilities.DefaultSkillDir
	Client     *core.Client
	// Log is the diagnostic file logger built by ensureConfig from the resolved
	// config's log level. It writes to a user-scoped file (never the TUI's
	// stdout/stderr). Tests may preset it (or leave it nil — slog calls on a nil
	// *slog.Logger are guarded at the call sites that can run pre-config).
	Log *slog.Logger
	// logCloser is the open log file's io.Closer, closed by App.Close on exit.
	logCloser io.Closer
	// BrowserOpener, when set, overrides how `login` opens the authorize URL
	// (used by tests; nil → the OS browser).
	BrowserOpener func(string) error
	// Interactive / LaunchTUI override TTY detection and the TUI launch (tests);
	// nil → real terminal check / real tui.Run.
	Interactive func() bool
	LaunchTUI   func(*App) error
}

// ensureConfig populates Cfg, Store, Log, and HTTP from disk/defaults WITHOUT
// resolving an environment. Used by config-management commands (env use) that
// run before any default_env exists. Idempotent; preset fields are kept.
func (a *App) ensureConfig() error {
	if a.Store == nil {
		a.Store = local.KeyringStore{}
	}
	if a.Cfg == nil {
		path := a.ConfigPath
		if path == "" {
			p, err := local.DefaultConfigPath()
			if err != nil {
				return err
			}
			path = p
		}
		cfg, err := local.Load(path)
		if err != nil {
			return err
		}
		a.Cfg = cfg
	}
	// Build the diagnostic file logger from the resolved config's level. A
	// failure to open the log file is non-fatal — the CLI must still run — so we
	// fall back to a discard logger and carry on. Only built once (tests may
	// preset a.Log to capture records).
	if a.Log == nil {
		a.initLogger()
	}
	// Default HTTP client wraps the kernel's widened-TLS transport in the
	// logging RoundTripper so every gateway/admin/auth call records per-phase
	// timings. Only set when nil — tests inject their own client to bypass real
	// network calls, and that injection must win.
	if a.HTTP == nil {
		base := core.NewHTTPTransport()
		// HTTP/2 PING health-checking: prod is h2, so all requests to a host share one
		// connection; without this a NAT-dropped connection is reused dead and every
		// request hangs to the timeout with no recovery. See local.EnableH2Health.
		local.EnableH2Health(base)
		// Reactive retry on top: prod closes each h2 connection at a max-age (~30
		// min). The dashboard polls keep that connection too "active" for the idle
		// PING to evict it, so the kill surfaces as a mid-request
		// "http2: client connection lost". RetryTransport re-sends idempotent GETs
		// once on a fresh connection so the hang is invisible. See local.RetryTransport.
		a.HTTP = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &local.RetryTransport{
				Next: &local.LoggingTransport{Base: base, Log: a.Log},
				Idle: base,
			},
		}
	}
	return nil
}

// initLogger opens the file logger and emits the session-start line. A failure
// to open the log (read-only home, etc.) is swallowed into a discard logger so
// the CLI keeps working — diagnostics are a convenience, never a hard dependency.
func (a *App) initLogger() {
	p, err := paths.DefaultPaths()
	if err != nil {
		a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		return
	}
	logger, closer, err := local.OpenLogger(p, a.Cfg.SlogLevel())
	if err != nil {
		a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		return
	}
	a.Log = logger
	a.logCloser = closer
	a.Log.Info("cli session start",
		"version", Version,
		"env", a.Cfg.DefaultEnv,
		"config", p.ConfigFile,
		"log_file", p.LogFile,
		"level", a.Cfg.SlogLevel().String(),
	)
}

// Close releases the open log file. It is called once from Main on process exit;
// a nil closer (logger never opened, or a test-injected logger) is a no-op.
func (a *App) Close() error {
	if a.logCloser != nil {
		return a.logCloser.Close()
	}
	return nil
}

// ensureEnv loads config and resolves the active environment (FR-5 precedence).
func (a *App) ensureEnv() error {
	if err := a.ensureConfig(); err != nil {
		return err
	}
	if a.Env.Name == "" {
		env, err := a.Cfg.Resolve(a.EnvFlag, "")
		if err != nil {
			return err
		}
		a.Env = env
	}
	return nil
}

// client returns the typed core client, building it from the resolved env when
// a test has not injected one.
func (a *App) client() *core.Client {
	if a.Client != nil {
		return a.Client
	}
	return core.NewClient(a.Env, core.NewTokenSource(a.Env, a.Store, a.HTTP), a.HTTP)
}

// loggedIn reports whether a usable credential (a JWT access token or an admin
// key) is stored for the resolved environment. The setup/login guard uses it to
// guide an unauthenticated operator instead of failing with a raw 401 later.
func (a *App) loggedIn() bool {
	if v, err := a.Store.Get(a.Env.Name, core.SecretAccessToken); err == nil && v != "" {
		return true
	}
	v, err := a.Store.Get(a.Env.Name, core.SecretAdminKey)
	return err == nil && v != ""
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// interactive reports whether stdout is a terminal (so the no-subcommand path
// launches the TUI rather than printing help). False under `go test` and pipes.
func (a *App) interactive() bool {
	if a.Interactive != nil {
		return a.Interactive()
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// renderJSON writes v as indented JSON to the output.
func (a *App) renderJSON(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printf writes a formatted line to the output.
func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }

// isJSON reports whether JSON output was requested.
func (a *App) isJSON() bool { return a.Format == "json" }

// vkSecret resolves the Virtual Key secret for VK-authed commands (chat /
// simulate): the --vk flag wins, otherwise the secret stored by the TUI wizard.
func (a *App) vkSecret(flagVK string) (string, error) {
	if flagVK != "" {
		return flagVK, nil
	}
	secret, err := a.Store.Get(a.Env.Name, core.SecretVKSecret)
	if err != nil || secret == "" {
		return "", fmt.Errorf("%w: no Virtual Key secret for env %q — pass --vk, or pick one in the TUI wizard", errUsage, a.Env.Name)
	}
	return secret, nil
}

// resolveModel picks the model slug for VK-authed commands: the --model flag
// wins, otherwise the remembered selection.
func (a *App) resolveModel(flagModel string) (string, error) {
	if flagModel != "" {
		return flagModel, nil
	}
	if a.Env.LastModel != "" {
		return a.Env.LastModel, nil
	}
	return "", fmt.Errorf("%w: no model selected — pass --model or pick one in the TUI wizard", errUsage)
}
