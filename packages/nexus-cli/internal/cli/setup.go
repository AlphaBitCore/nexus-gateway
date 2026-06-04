package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// newSetupCmd is the friendly interactive environment setup. `nexus setup [name]`
// creates or edits an environment's non-secret settings, sets it as the default,
// and points the operator at `nexus login` next. It never writes secrets.
func newSetupCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "setup [name]",
		Short:       "Interactively create or edit an environment/profile",
		Long:        "Walks through an environment's Control Plane URL, AI Gateway URL, OAuth client, and prod flag, saves it, and sets it as the default. Run `nexus login` afterwards to authenticate.",
		Annotations: map[string]string{"skipLoad": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.ensureConfig(); err != nil {
				return err
			}
			r := bufio.NewReader(cmd.InOrStdin())
			ask := func(label, def string) string {
				if def != "" {
					fmt.Fprintf(a.ErrOut, "%s [%s]: ", label, def)
				} else {
					fmt.Fprintf(a.ErrOut, "%s: ", label)
				}
				line, _ := r.ReadString('\n')
				if line = strings.TrimSpace(line); line == "" {
					return def
				}
				return line
			}

			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			if name == "" {
				name = ask("Environment name", "dev")
			}
			if name == "" {
				return fmt.Errorf("%w: environment name is required", errUsage)
			}

			cur := a.Cfg.Envs[name] // zero value when new
			env := core.Env{
				Name:             name,
				CPBaseURL:        ask("Control Plane base URL", cur.CPBaseURL),
				AIGatewayBaseURL: ask("AI Gateway base URL", orDefault(cur.AIGatewayBaseURL, cur.CPBaseURL)),
				OAuthClientID:    ask("OAuth client id", orDefault(cur.OAuthClientID, "tui")),
				OAuthRedirectURI: ask("OAuth redirect URI", orDefault(cur.OAuthRedirectURI, "http://localhost:3000/auth/callback")),
				IsProd:           askYesNo(ask, "Production environment? (red banner + confirmations)", cur.IsProd),
				// Preserve the remembered model/VK selection when editing.
				LastModel:  cur.LastModel,
				LastVKID:   cur.LastVKID,
				LastVKName: cur.LastVKName,
			}
			if env.CPBaseURL == "" {
				return fmt.Errorf("%w: Control Plane base URL is required", errUsage)
			}

			a.Cfg.SetEnv(env)
			if err := a.Cfg.SetDefault(name); err != nil {
				return err
			}
			if err := a.Cfg.Save(); err != nil {
				return err
			}
			a.printf("Saved environment %q and set it as the default.\nNext: run `nexus login` (browser sign-in) or `nexus login --admin-key` (machine) to authenticate.\n", name)
			return nil
		},
	}
}

// askYesNo prompts a yes/no question with the current value as the default.
func askYesNo(ask func(label, def string) string, label string, cur bool) bool {
	def := "N"
	if cur {
		def = "y"
	}
	ans := strings.ToLower(ask(label+" (y/N)", def))
	return ans == "y" || ans == "yes"
}
