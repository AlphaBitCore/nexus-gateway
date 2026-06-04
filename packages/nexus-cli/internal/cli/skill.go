package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/skills"
)

// newSkillCmd builds `nexus skill` — list the agent's skills and install new
// ones from a URL. Skills are local files (built-ins are embedded); installing
// is a download → checksum → human-review → confirm flow, so an unreviewed skill
// is never made active. The command manages local files only — no env/login
// needed (skipLoad).
func newSkillCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "skill",
		Short:       "List and install agent skills (progressive-disclosure playbooks)",
		Annotations: map[string]string{"skipLoad": "true"},
	}
	cmd.AddCommand(newSkillLsCmd(a), newSkillInstallCmd(a))
	return cmd
}

// skillDir is the local skill directory: the test override, else the default
// ~/.config/nexus/skills.
func (a *App) skillDir() (string, error) {
	if a.SkillDir != "" {
		return a.SkillDir, nil
	}
	return capabilities.DefaultSkillDir()
}

// skillSummary is the JSON shape for `skill ls`.
type skillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func newSkillLsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List available skills (built-in + locally installed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := a.skillDir()
			if err != nil {
				return err
			}
			set, err := skills.Load(dir)
			if err != nil {
				return err
			}
			names := set.Names()
			rows := make([]skillSummary, 0, len(names))
			for _, n := range names {
				sk, _ := set.Get(n)
				rows = append(rows, skillSummary{Name: sk.Name, Description: sk.Description})
			}
			if a.isJSON() {
				return a.renderJSON(rows)
			}
			cells := make([][]string, 0, len(rows))
			for _, r := range rows {
				cells = append(cells, []string{r.Name, r.Description})
			}
			return a.table([]string{"NAME", "DESCRIPTION"}, cells)
		},
	}
}

func newSkillInstallCmd(a *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "install <url>",
		Short: "Download a skill, show it for review, and install it with --yes",
		Long: "Downloads a skill file from a URL and prints its name, description, " +
			"SHA-256 checksum, and body for review. Nothing is written until you " +
			"re-run with --yes to confirm — an unreviewed skill is never made active.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: skill install requires exactly one <url>", errUsage)
			}
			dir, err := a.skillDir()
			if err != nil {
				return err
			}
			info, err := skills.Preview(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			a.printf("Skill:       %s\n", info.Name)
			a.printf("Description: %s\n", info.Description)
			a.printf("SHA-256:     %s\n", info.SHA256)
			a.printf("\n%s\n\n", info.Body)
			if !yes {
				a.printf("Reviewed? Re-run with --yes to install into %s\n", dir)
				return nil
			}
			dest, err := skills.InstallFetched(info, dir)
			if err != nil {
				return err
			}
			a.printf("Installed %q to %s\n", info.Name, dest)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm installation after reviewing the skill")
	return cmd
}
