package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

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
