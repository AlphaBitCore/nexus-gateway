package cli

import (
	"fmt"
	"net/url"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newCostCmd(a *App) *cobra.Command {
	var groupBy string
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Show cost grouped by provider/user/model",
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if groupBy != "" {
				q.Set("groupBy", groupBy)
			}
			rep, err := a.client().Cost(cmd.Context(), q)
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(rep)
			}
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "GROUP\tREQUESTS\tTOKENS\tCOST_USD\tCACHE_HITS")
			var totalCost float64
			for _, r := range rep.Data {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%.4f\t%d\n", r.GroupLabel, r.RequestCount, r.TotalTokens, r.TotalCostUSD, r.CacheHitCount)
				totalCost += r.TotalCostUSD
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			a.printf("\ntotal cost: %.4f USD across %d groups\n", totalCost, len(rep.Data))
			return nil
		},
	}
	cmd.Flags().StringVar(&groupBy, "group", "provider", "group by: provider | user | model | device")
	return cmd
}
