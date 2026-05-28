package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui"
)

// NewRootCmd builds the `nexus` command tree bound to a.
func NewRootCmd(a *App) *cobra.Command {
	root := &cobra.Command{
		Use:           "nexus",
		Short:         "Operate and observe the Nexus Gateway from the terminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The bare `nexus` (no subcommand) launches the TUI, which runs its own
		// entry wizard (login + model/VK), so it resolves an env but needs no
		// stored credential up front.
		Annotations: map[string]string{"skipAuth": "true"},
		// Load config + resolve env before any subcommand runs, then (for gateway
		// commands) ensure the operator is set up and logged in — guiding them to
		// `nexus setup` / `nexus login` rather than failing later with a raw 401.
		// `skipLoad` commands manage config without a live env (env, setup);
		// `skipAuth` commands resolve an env but need no credential (login).
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Annotations["skipLoad"] == "true" {
				return nil
			}
			if err := a.ensureEnv(); err != nil {
				return fmt.Errorf("%w: %v — run `nexus setup` to configure an environment (its Control Plane URL, etc.), then `nexus login`", core.ErrUnauthorized, err)
			}
			if cmd.Annotations["skipAuth"] == "true" {
				return nil
			}
			if !a.loggedIn() {
				return fmt.Errorf("%w: not logged in to %q — run `nexus login` for browser sign-in, or `nexus login --admin-key` for a machine profile (or `nexus setup` to configure a different environment)", core.ErrUnauthorized, a.Env.Name)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// With no subcommand: launch the TUI on an interactive terminal,
			// otherwise print help (so piped/non-TTY invocations stay scriptable).
			if a.interactive() {
				return a.launchTUI()
			}
			return cmd.Help()
		},
	}
	root.PersistentFlags().StringVar(&a.EnvFlag, "env", "", "environment/profile to target (overrides default_env)")
	root.PersistentFlags().StringVarP(&a.Format, "output", "o", "table", "output format: table | json")

	root.AddCommand(
		newSetupCmd(a),
		newLoginCmd(a),
		newEnvCmd(a),
		newModelsCmd(a),
		newTrafficCmd(a),
		newHealthCmd(a),
		newCostCmd(a),
		newSLOCmd(a),
		newChatCmd(a),
		newSimulateCmd(a),
		newRouteCmd(a),
		newKillSwitchCmd(a),
		newPassthroughCmd(a),
		newVKCmd(a),
		newMCPCmd(a),
	)
	return root
}

// launchTUI starts the operator console (or the injected test hook).
func (a *App) launchTUI() error {
	if a.LaunchTUI != nil {
		return a.LaunchTUI(a)
	}
	return tui.Run(a.tuiDeps())
}

// tuiSession builds the dashboard session from the currently-resolved env plus
// the VK secret stored for it. The closures below read a.Env (not a captured
// copy) so a wizard env switch is reflected everywhere.
func (a *App) tuiSession() tui.Session {
	vkSecret, _ := a.Store.Get(a.Env.Name, core.SecretVKSecret)
	return tui.Session{
		EnvName:  a.Env.Name,
		Addr:     a.Env.CPBaseURL,
		IsProd:   a.Env.IsProd,
		Model:    a.Env.LastModel,
		VKID:     a.Env.LastVKID,
		VKName:   a.Env.LastVKName,
		VKSecret: vkSecret,
	}
}

// tuiDeps assembles the shell dependencies: the typed gateway plus the
// auth/persistence callbacks the entry wizard needs (env switch/create, login,
// VK-secret storage, remembered-selection persistence). All reach the gateway
// only through core. The closures read a.Env dynamically so the env step's
// switch/create (which mutates a.Env and rebuilds the client) takes effect.
func (a *App) tuiDeps() tui.Deps {
	names := make([]string, 0, len(a.Cfg.Envs))
	for n := range a.Cfg.Envs {
		names = append(names, n)
	}
	sort.Strings(names)
	return tui.Deps{
		Gateway:  a.client(),
		Session:  a.tuiSession(),
		EnvNames: names,
		HasSession: func() bool {
			return a.loggedIn()
		},
		SwitchEnv: func(name string) (tui.Gateway, tui.Session, bool, error) {
			env, err := a.Cfg.Resolve(name, "")
			if err != nil {
				return nil, tui.Session{}, false, err
			}
			a.Env, a.Client = env, nil // force the client to rebuild for the new env
			return a.client(), a.tuiSession(), a.loggedIn(), nil
		},
		CreateEnv: func(name, cpBaseURL string, prod bool) (tui.Gateway, tui.Session, error) {
			env := core.Env{
				Name:             name,
				CPBaseURL:        cpBaseURL,
				AIGatewayBaseURL: cpBaseURL,
				OAuthClientID:    "cp-ui",
				OAuthRedirectURI: "http://localhost:3000/auth/callback",
				IsProd:           prod,
			}
			a.Cfg.SetEnv(env)
			if err := a.Cfg.SetDefault(name); err != nil {
				return nil, tui.Session{}, err
			}
			if err := a.Cfg.Save(); err != nil {
				return nil, tui.Session{}, err
			}
			a.Env, a.Client = env, nil
			return a.client(), a.tuiSession(), nil
		},
		Login: func(ctx context.Context) error {
			return core.NewAuthenticator(a.Env, a.Store, a.HTTP).
				WithBrowserOpener(a.BrowserOpener).LoginBrowser(ctx)
		},
		SaveVKSecret: func(secret string) error {
			return a.Store.Set(a.Env.Name, core.SecretVKSecret, secret)
		},
		SaveSelection: func(model, vkID, vkName string) error {
			a.Env.LastModel, a.Env.LastVKID, a.Env.LastVKName = model, vkID, vkName
			a.Cfg.SetEnv(a.Env)
			return a.Cfg.Save()
		},
		CreateVK: func(ctx context.Context, name string) (string, string, string, error) {
			vk, err := a.client().CreateVK(ctx, name)
			if err != nil {
				return "", "", "", err
			}
			return vk.ID, vk.Name, vk.Key, nil
		},
	}
}

// Main is the process entry point: it builds the app, executes, and returns the
// documented exit code.
func Main() int {
	a := &App{Out: os.Stdout, ErrOut: os.Stderr}
	root := NewRootCmd(a)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(a.ErrOut, "error:", err)
		return exitCode(err)
	}
	return 0
}

// exitCode maps an error to the documented process exit status.
//
//	0 success · 1 generic/transport · 2 usage · 3 auth required ·
//	4 IAM denied (403) · 5 not found (404)
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, core.ErrUnauthorized):
		return 3
	case errors.Is(err, core.ErrForbidden):
		return 4
	case errors.Is(err, core.ErrNotFound):
		return 5
	default:
		return 1
	}
}
