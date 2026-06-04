package cli

import (
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/spf13/cobra"
)

// --- killswitch ---

func newKillSwitchCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "killswitch",
		Short: "Show or toggle the global kill switch",
		Long: "Reads or toggles the global kill switch (halts TLS bumping on every " +
			"node) via the admin API. `status` reads the current engaged state; the " +
			"on/off subcommands toggle it and require --yes in a prod environment.",
	}
	cmd.AddCommand(newKillSwitchStatusCmd(a), newKillSwitchSetCmd(a, true), newKillSwitchSetCmd(a, false))
	return cmd
}

// newKillSwitchStatusCmd reads the current kill-switch state from the config-sync
// history (the kill-switch route is write-only; its state lives there).
func newKillSwitchStatusCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the current kill-switch state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.client().KillSwitchStatus(cmd.Context())
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(st)
			}
			if !st.Known {
				a.printf("kill switch: never toggled (off)\n")
				return nil
			}
			a.printf("kill switch engaged=%v (version %d, last toggled by %s)\n", st.Engaged, st.Version, dashCLI(st.By))
			return nil
		},
	}
}

// dashCLI renders an em dash for an empty string in CLI output.
func dashCLI(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// newKillSwitchSetCmd builds the `on` or `off` subcommand.
func newKillSwitchSetCmd(a *App, engage bool) *cobra.Command {
	use := "off"
	short := "Disengage the kill switch"
	if engage {
		use, short = "on", "Engage the kill switch"
	}
	var yes bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.Env.IsProd && !yes {
				return fmt.Errorf("%w: refusing to toggle the kill switch in prod without --yes", errUsage)
			}
			res, err := a.client().SetKillSwitch(cmd.Context(), engage)
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(res)
			}
			a.printf("kill switch engaged=%v (version %d, notified %d/%d things online)\n",
				res.Engaged, res.Version, res.ThingsNotified, res.ThingsOnline)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the toggle (required in prod)")
	return cmd
}

// --- passthrough ---

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

// --- route ---

// newRouteCmd builds `nexus route explain`, the routing dry-run ("why this
// route"). It fires no real request.
func newRouteCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Inspect routing decisions",
	}
	cmd.AddCommand(newRouteExplainCmd(a))
	return cmd
}

func newRouteExplainCmd(a *App) *cobra.Command {
	var model, endpoint string
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show which provider/model a request resolves to (dry-run)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if model == "" {
				return fmt.Errorf("%w: --model is required", errUsage)
			}
			res, err := a.client().RoutingSimulate(cmd.Context(), core.RoutingSimulateRequest{
				ModelID: model, EndpointType: endpoint,
			})
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(res)
			}
			if res.Substituted {
				a.printf("substituted: yes (rule %q)\n", res.RuleName)
			} else {
				a.printf("substituted: no\n")
			}
			a.printf("\ntargets:\n")
			printTargets(a, res.Targets)
			if len(res.RecoveryTargets) > 0 {
				a.printf("\nrecovery:\n")
				printTargets(a, res.RecoveryTargets)
			}
			if len(res.Warnings) > 0 {
				a.printf("\nwarnings:\n")
				for _, w := range res.Warnings {
					a.printf("  ⚠ %s\n", w)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "model slug to resolve (required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "chat", "endpoint type: chat | responses | messages")
	return cmd
}

func printTargets(a *App, targets []core.RoutingTarget) {
	if len(targets) == 0 {
		a.printf("  (none — request would be rejected by the router)\n")
		return
	}
	for _, t := range targets {
		a.printf("  %s → %s\n", t.ProviderName, t.ModelCode)
	}
}

// --- vk ---

// newVKCmd builds `nexus vk create`, the way a CLI operator obtains a Virtual
// Key they own (VK secrets are stored hashed, so the only way to get a usable
// key is to create one and capture the once-returned plaintext).
func newVKCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vk",
		Short: "Manage your Virtual Keys",
	}
	cmd.AddCommand(newVKCreateCmd(a))
	return cmd
}

func newVKCreateCmd(a *App) *cobra.Command {
	var name string
	var store bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a personal Virtual Key you own (prints the secret once)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			vk, err := a.client().CreateVK(cmd.Context(), name)
			if err != nil {
				return err
			}
			if store {
				if err := a.Store.Set(a.Env.Name, core.SecretVKSecret, vk.Key); err != nil {
					return err
				}
				a.Env.LastVKID, a.Env.LastVKName = vk.ID, vk.Name
				a.Cfg.SetEnv(a.Env)
				if err := a.Cfg.Save(); err != nil {
					return err
				}
			}
			if a.isJSON() {
				return a.renderJSON(vk)
			}
			a.printf("Created Virtual Key %q (%s)\n", vk.Name, vk.ID)
			a.printf("secret (shown once): %s\n", vk.Key)
			if store {
				a.printf("stored as this env's Virtual Key (keychain).\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "nexus-cli", "name for the new Virtual Key")
	cmd.Flags().BoolVar(&store, "store", true, "store the secret in the keychain + remember it for this env")
	return cmd
}
