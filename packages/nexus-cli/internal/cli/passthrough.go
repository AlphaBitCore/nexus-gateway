package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

func newPassthroughCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "passthrough",
		Short: "Show or toggle emergency passthrough (bypass compliance hooks)",
		Long: "Reads the three-tier emergency-passthrough state (global + per-adapter " +
			"+ per-provider overrides) or toggles the global tier. The global on/off " +
			"subcommands require --yes in a prod environment.",
	}
	cmd.AddCommand(newPassthroughStatusCmd(a), newPassthroughGlobalCmd(a))
	return cmd
}

// newPassthroughStatusCmd reads the three-tier emergency-passthrough snapshot.
func newPassthroughStatusCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the emergency-passthrough snapshot",
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, err := a.client().PassthroughSnapshot(cmd.Context())
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(snap)
			}
			g := snap.Global
			gEngaged := g.Enabled && (g.BypassHooks || g.BypassCache || g.BypassNormalize)
			a.printf("global passthrough: %s\n", onOff(gEngaged))
			adapters, providers := snap.ActiveOverrides()
			a.printf("active overrides: %d adapter(s), %d provider(s)\n", adapters, providers)
			return nil
		},
	}
}

func newPassthroughGlobalCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "global",
		Short: "Set the global emergency-passthrough tier",
	}
	cmd.AddCommand(newPassthroughGlobalSetCmd(a, true), newPassthroughGlobalSetCmd(a, false))
	return cmd
}

// newPassthroughGlobalSetCmd builds the global `on` or `off` subcommand. Engaging
// bypasses the compliance hooks by default (the canonical emergency); --bypass-cache
// and --bypass-normalize add the other tiers.
func newPassthroughGlobalSetCmd(a *App, engage bool) *cobra.Command {
	use, short := "off", "Disengage global emergency passthrough"
	if engage {
		use, short = "on", "Engage global emergency passthrough (bypass hooks)"
	}
	var yes, bypassCache, bypassNormalize bool
	var reason string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.Env.IsProd && !yes {
				return fmt.Errorf("%w: refusing to toggle global passthrough in prod without --yes", errUsage)
			}
			req := core.PassthroughGlobalRequest{Enabled: engage, Reason: reason}
			if engage {
				req.BypassHooks = true
				req.BypassCache = bypassCache
				req.BypassNormalize = bypassNormalize
			}
			if err := a.client().SetPassthroughGlobal(cmd.Context(), req); err != nil {
				return err
			}
			a.printf("global emergency passthrough %s\n", onOff(engage))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the change (required in prod)")
	cmd.Flags().StringVar(&reason, "reason", "", "reason recorded with the change")
	if engage {
		cmd.Flags().BoolVar(&bypassCache, "bypass-cache", false, "also bypass the response cache")
		cmd.Flags().BoolVar(&bypassNormalize, "bypass-normalize", false, "also bypass normalization")
	}
	return cmd
}

// onOff renders a boolean as ON/OFF for CLI output.
func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}
