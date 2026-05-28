package cli

import (
	"fmt"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// sloReport is the combined JSON shape for `nexus slo`.
type sloReport struct {
	Env            string                 `json:"env"`
	Requests       float64                `json:"requests"`
	Errors         float64                `json:"errors"`
	AvailabilityPc float64                `json:"availabilityPct"`
	Providers      []core.LatencyPhaseRow `json:"providers"`
	Fallbacks      []core.FallbackRow     `json:"fallbacks"`
}

// newSLOCmd prints the Performance/SLO summary: overall availability, the
// per-provider latency-percentile table, and routing-fallback activity.
func newSLOCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "slo",
		Short: "Show provider SLO: availability, p95 latency, fallbacks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := a.client()
			now := time.Now().UTC()
			win := url.Values{
				"start": {now.AddDate(0, 0, -7).Format(time.RFC3339)},
				"end":   {now.Format(time.RFC3339)},
			}
			lp, err := c.LatencyPhases(cmd.Context(), "provider", win)
			if err != nil {
				return err
			}
			fb, err := c.RoutingFallbacks(cmd.Context(), win)
			if err != nil {
				return err
			}
			sp, err := c.Sparkline(cmd.Context(), nil)
			if err != nil {
				return err
			}
			tot := sp.Totals()
			reqs := tot[core.MetricRequestCount]
			errs := tot[core.MetricStatus4xxCount] + tot[core.MetricStatus5xxCount]
			avail := 100.0
			if reqs > 0 {
				avail = 100 - errs/reqs*100
			}
			rep := sloReport{Env: a.Env.Name, Requests: reqs, Errors: errs, AvailabilityPc: avail, Providers: lp.Rows, Fallbacks: fb.Data}
			if a.isJSON() {
				return a.renderJSON(rep)
			}
			a.printf("environment:  %s\navailability: %.2f%%  (requests %.0f, errors %.0f)\n\n", rep.Env, rep.AvailabilityPc, rep.Requests, rep.Errors)
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROVIDER\tREQS\tP50_MS\tP95_MS\tTTFB_P95_MS")
			for _, r := range rep.Providers {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", r.GroupLabel, r.RequestCount, r.TotalP50Ms, r.TotalP95Ms, r.UpstreamTTFBP95Ms)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if len(rep.Fallbacks) > 0 {
				a.printf("\nrouting fallbacks:\n")
				for _, f := range rep.Fallbacks {
					label := f.GroupLabel
					if label == "" {
						label = f.Group
					}
					a.printf("  %-30s %d\n", label, f.RequestCount)
				}
			}
			return nil
		},
	}
}
