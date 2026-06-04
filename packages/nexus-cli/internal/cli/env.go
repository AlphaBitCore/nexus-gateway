package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func newEnvCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "env",
		Short:       "List or switch environments/profiles",
		Annotations: map[string]string{"skipLoad": "true"},
	}
	cmd.AddCommand(newEnvLsCmd(a), newEnvUseCmd(a), newEnvAddCmd(a), newEnvRmCmd(a))
	return cmd
}

// newEnvAddCmd adds (or overwrites) an environment from flags — the scriptable
// counterpart to the interactive `nexus setup`.
func newEnvAddCmd(a *App) *cobra.Command {
	var cpURL, aigwURL, clientID, redirect string
	var prod bool
	cmd := &cobra.Command{
		Use:         "add <name>",
		Short:       "Add or overwrite an environment from flags (scriptable)",
		Annotations: map[string]string{"skipLoad": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: env add requires exactly one <name>", errUsage)
			}
			if cpURL == "" {
				return fmt.Errorf("%w: --cp-url is required", errUsage)
			}
			if err := a.ensureConfig(); err != nil {
				return err
			}
			name := args[0]
			a.Cfg.SetEnv(core.Env{
				Name:             name,
				CPBaseURL:        cpURL,
				AIGatewayBaseURL: orDefault(aigwURL, cpURL),
				OAuthClientID:    orDefault(clientID, "tui"),
				OAuthRedirectURI: orDefault(redirect, "http://localhost:3000/auth/callback"),
				IsProd:           prod,
			})
			if a.Cfg.DefaultEnv == "" {
				a.Cfg.DefaultEnv = name
			}
			if err := a.Cfg.Save(); err != nil {
				return err
			}
			a.printf("Added environment %q. Run `nexus login` to authenticate.\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&cpURL, "cp-url", "", "Control Plane base URL (required)")
	cmd.Flags().StringVar(&aigwURL, "aigw-url", "", "AI Gateway base URL (defaults to --cp-url)")
	cmd.Flags().StringVar(&clientID, "oauth-client", "", "OAuth client id (default tui)")
	cmd.Flags().StringVar(&redirect, "oauth-redirect", "", "OAuth redirect URI")
	cmd.Flags().BoolVar(&prod, "prod", false, "mark as a production environment (red banner + confirmations)")
	return cmd
}

// newEnvRmCmd removes an environment.
func newEnvRmCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "rm <name>",
		Short:       "Remove an environment",
		Annotations: map[string]string{"skipLoad": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: env rm requires exactly one <name>", errUsage)
			}
			if err := a.ensureConfig(); err != nil {
				return err
			}
			if err := a.Cfg.RemoveEnv(args[0]); err != nil {
				return err
			}
			if err := a.Cfg.Save(); err != nil {
				return err
			}
			a.printf("Removed environment %q.\n", args[0])
			return nil
		},
	}
}

// envSummary is the JSON shape for `env ls`.
type envSummary struct {
	Name      string `json:"name"`
	CPBaseURL string `json:"cpBaseUrl"`
	IsProd    bool   `json:"isProd"`
	IsDefault bool   `json:"isDefault"`
}

func newEnvLsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "ls",
		Short:       "List configured environments",
		Annotations: map[string]string{"skipLoad": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.ensureConfig(); err != nil {
				return err
			}
			names := make([]string, 0, len(a.Cfg.Envs))
			for n := range a.Cfg.Envs {
				names = append(names, n)
			}
			sort.Strings(names)
			rows := make([]envSummary, 0, len(names))
			for _, n := range names {
				e := a.Cfg.Envs[n]
				rows = append(rows, envSummary{Name: n, CPBaseURL: e.CPBaseURL, IsProd: e.IsProd, IsDefault: n == a.Cfg.DefaultEnv})
			}
			if a.isJSON() {
				return a.renderJSON(rows)
			}
			cells := make([][]string, 0, len(rows))
			for _, r := range rows {
				def := ""
				if r.IsDefault {
					def = "*"
				}
				cells = append(cells, []string{r.Name, r.CPBaseURL, fmt.Sprintf("%v", r.IsProd), def})
			}
			return a.table([]string{"NAME", "CP URL", "PROD", "DEFAULT"}, cells)
		},
	}
}

func newEnvUseCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "use <name>",
		Short:       "Set the default environment",
		Annotations: map[string]string{"skipLoad": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: env use requires exactly one <name>", errUsage)
			}
			if err := a.ensureConfig(); err != nil {
				return err
			}
			if err := a.Cfg.SetDefault(args[0]); err != nil {
				return err
			}
			if err := a.Cfg.Save(); err != nil {
				return err
			}
			a.printf("Default environment set to %q.\n", args[0])
			return nil
		},
	}
}
