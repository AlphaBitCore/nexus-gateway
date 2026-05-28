package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newModelsCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{Use: "models", Short: "Model catalog"}
	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List configured models",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := a.client().AdminModels(cmd.Context())
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(cat)
			}
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CODE\tNAME\tPROVIDER\tTYPE\tENABLED")
			for _, g := range cat.Data {
				for _, m := range g.Models {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\n", m.Code, m.Name, g.Provider.Label(), m.Type, m.Enabled)
				}
			}
			return tw.Flush()
		},
	})
	return cmd
}
