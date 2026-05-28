package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

func newTrafficCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{Use: "traffic", Short: "Inspect traffic events"}
	cmd.AddCommand(newTrafficLsCmd(a), newTrafficGetCmd(a))
	return cmd
}

func newTrafficLsCmd(a *App) *cobra.Command {
	var (
		status, provider, model, vk string
		since                       time.Duration
		limit                       int
	)
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List traffic events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := core.TrafficFilter{
				StatusRange:  status,
				Provider:     provider,
				ModelUsed:    model,
				VirtualKeyID: vk,
				Limit:        limit,
			}
			if since > 0 {
				f.StartTime = time.Now().Add(-since)
			}
			list, err := a.client().TrafficList(cmd.Context(), f)
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(list)
			}
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tSTATUS\tMODEL\tTOKENS\tCOST_USD\tID")
			for _, e := range list.Data {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%.6f\t%s\n",
					e.Timestamp.Format(time.RFC3339), e.StatusCode, e.ModelName, e.TotalTokens, e.EstCostUSD, e.ID)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			a.printf("\n%d of %d events\n", len(list.Data), list.Total)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "status range filter (e.g. 4xx, 5xx)")
	cmd.Flags().StringVar(&provider, "provider", "", "provider id filter")
	cmd.Flags().StringVar(&model, "model", "", "model filter")
	cmd.Flags().StringVar(&vk, "vk", "", "virtual key id filter")
	cmd.Flags().DurationVar(&since, "since", 0, "only events newer than this (e.g. 1h, 30m)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max events to return")
	return cmd
}

func newTrafficGetCmd(a *App) *cobra.Command {
	var normalized bool
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show one traffic event",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: traffic get requires exactly one <id>", errUsage)
			}
			if normalized {
				raw, err := a.client().TrafficEventNormalized(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				a.printf("%s\n", string(raw))
				return nil
			}
			ev, err := a.client().TrafficEvent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(ev)
			}
			a.printf("id:       %s\nstatus:   %d\nmodel:    %s (%s)\ntokens:   %d (prompt %d / completion %d)\ncost USD: %.6f\ncache:    %s\ntrace:    %s\nlatency:  total=%dms ttfb=%dms upstream=%dms reqHooks=%dms respHooks=%dms\n",
				ev.ID, ev.StatusCode, ev.ModelName, ev.ProviderName, ev.TotalTokens, ev.PromptTokens, ev.CompletionTok,
				ev.EstCostUSD, ev.CacheStatus, ev.TraceID, ev.LatencyMs, ev.UpstreamTTFBMs, ev.UpstreamTotMs, ev.RequestHooksMs, ev.RespHooksMs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&normalized, "normalized", false, "show the normalized view instead of the raw event")
	return cmd
}
