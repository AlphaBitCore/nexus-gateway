package cli

import (
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/spf13/cobra"
	"net/url"
	"sort"
	"time"
)

// --- cost ---

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
			var totalCost float64
			rows := make([][]string, 0, len(rep.Data))
			for _, r := range rep.Data {
				rows = append(rows, []string{r.GroupLabel, fmt.Sprintf("%d", r.RequestCount), fmt.Sprintf("%d", r.TotalTokens), fmt.Sprintf("%.4f", r.TotalCostUSD), fmt.Sprintf("%d", r.CacheHitCount)})
				totalCost += r.TotalCostUSD
			}
			if err := a.table([]string{"GROUP", "REQUESTS", "TOKENS", "COST_USD", "CACHE_HITS"}, rows); err != nil {
				return err
			}
			a.printf("\ntotal cost: %.4f USD across %d groups\n", totalCost, len(rep.Data))
			return nil
		},
	}
	cmd.Flags().StringVar(&groupBy, "group", "provider", "group by: provider | user | model | device")
	return cmd
}

// --- slo ---

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
			rows := make([][]string, 0, len(rep.Providers))
			for _, r := range rep.Providers {
				rows = append(rows, []string{r.GroupLabel, fmt.Sprintf("%d", r.RequestCount), fmt.Sprintf("%d", r.TotalP50Ms), fmt.Sprintf("%d", r.TotalP95Ms), fmt.Sprintf("%d", r.UpstreamTTFBP95Ms)})
			}
			if err := a.table([]string{"PROVIDER", "REQS", "P50_MS", "P95_MS", "TTFB_P95_MS"}, rows); err != nil {
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

// --- health ---

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
			names := make([]string, 0, len(rep.Services))
			for n := range rep.Services {
				names = append(names, n)
			}
			sort.Strings(names)
			rows := make([][]string, 0, len(names))
			for _, n := range names {
				rows = append(rows, []string{n, fmt.Sprintf("%d", rep.Services[n].Total)})
			}
			if err := a.table([]string{"SERVICE", "INSTANCES"}, rows); err != nil {
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

// --- traffic ---

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
			rows := make([][]string, 0, len(list.Data))
			for _, e := range list.Data {
				rows = append(rows, []string{
					e.Timestamp.Format(time.RFC3339), fmt.Sprintf("%d", e.StatusCode), e.ModelName,
					fmt.Sprintf("%d", e.TotalTokens), fmt.Sprintf("%.6f", e.EstCostUSD), e.ID})
			}
			if err := a.table([]string{"TIME", "STATUS", "MODEL", "TOKENS", "COST_USD", "ID"}, rows); err != nil {
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

// --- models ---

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
			var rows [][]string
			for _, g := range cat.Data {
				for _, m := range g.Models {
					rows = append(rows, []string{m.Code, m.Name, g.Provider.Label(), m.Type, fmt.Sprintf("%v", m.Enabled)})
				}
			}
			return a.table([]string{"CODE", "NAME", "PROVIDER", "TYPE", "ENABLED"}, rows)
		},
	})
	return cmd
}
