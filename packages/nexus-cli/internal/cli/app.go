// Package cli wires the `nexus` command tree (Cobra) over internal/core. Each
// command is a thin presenter: it calls a typed core capability and renders the
// result as a table (default) or stable JSON (--output json).
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// errUsage marks an argument/usage error so the exit-code mapper returns 2.
var errUsage = errors.New("usage error")

// App holds the shared state every command needs. Tests construct an App with
// Client/Out/Format preset to bypass config loading and real auth.
type App struct {
	Cfg        *core.Config
	Env        core.Env
	Store      core.SecretStore
	HTTP       *http.Client
	Out        io.Writer
	ErrOut     io.Writer
	Format     string // "table" | "json"
	EnvFlag    string // value of --env
	ConfigPath string // override for tests; empty → DefaultConfigPath
	Client     *core.Client
	// BrowserOpener, when set, overrides how `login` opens the authorize URL
	// (used by tests; nil → the OS browser).
	BrowserOpener func(string) error
	// Interactive / LaunchTUI override TTY detection and the TUI launch (tests);
	// nil → real terminal check / real tui.Run.
	Interactive func() bool
	LaunchTUI   func(*App) error
}

// ensureConfig populates Cfg, Store, and HTTP from disk/defaults WITHOUT
// resolving an environment. Used by config-management commands (env use) that
// run before any default_env exists. Idempotent; preset fields are kept.
func (a *App) ensureConfig() error {
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if a.Store == nil {
		a.Store = core.KeyringStore{}
	}
	if a.Cfg == nil {
		path := a.ConfigPath
		if path == "" {
			p, err := core.DefaultConfigPath()
			if err != nil {
				return err
			}
			path = p
		}
		cfg, err := core.Load(path)
		if err != nil {
			return err
		}
		a.Cfg = cfg
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
