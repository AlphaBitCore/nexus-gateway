package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/restable"
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
		newResourceCmd(a),
	)
	return root
}

// Main is the process entry point: it builds the app, executes, and returns the
// documented exit code.
func Main() int {
	a := &App{Out: os.Stdout, ErrOut: os.Stderr, In: os.Stdin}
	// Close the diagnostic log file on exit. ensureConfig (run in the command
	// tree's PersistentPreRunE) opens it; Close is a no-op when it never did.
	defer func() { _ = a.Close() }()
	root := NewRootCmd(a)
	if err := root.Execute(); err != nil {
		// The error string can embed a server-supplied response body (a 4xx/5xx body
		// is wrapped into the transport error). Sanitize it so an attacker-controlled
		// body cannot inject terminal escape sequences on the error path.
		fmt.Fprintln(a.ErrOut, "error:", restable.SanitizeTerminal(err.Error()))
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
