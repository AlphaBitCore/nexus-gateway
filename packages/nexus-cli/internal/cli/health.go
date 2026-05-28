package cli

import (
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// healthReport is the combined JSON shape for `nexus health`.
type healthReport struct {
	Env      string                         `json:"env"`
	Summary  map[string]float64             `json:"summary"`
	Services map[string]core.ServiceSummary `json:"services"`
	Nodes    int                            `json:"nodes"`
}

func newHealthCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Show gateway health (tiles + service/node status)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := a.client()
			sp, err := c.Sparkline(cmd.Context(), nil)
			if err != nil {
				return err
			}
			inst, err := c.Instances(cmd.Context())
			if err != nil {
				return err
			}
			rep := healthReport{Env: a.Env.Name, Summary: sp.Totals(), Services: inst.Services, Nodes: inst.Count}
			if a.isJSON() {
				return a.renderJSON(rep)
			}
			a.printf("environment: %s\nnodes:       %d\n\n", rep.Env, rep.Nodes)
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tINSTANCES")
			names := make([]string, 0, len(rep.Services))
			for n := range rep.Services {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintf(tw, "%s\t%d\n", n, rep.Services[n].Total)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if len(rep.Summary) > 0 {
				a.printf("\n")
				keys := make([]string, 0, len(rep.Summary))
				for k := range rep.Summary {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					a.printf("%-24s %.4f\n", k, rep.Summary[k])
				}
			}
			return nil
		},
	}
}
