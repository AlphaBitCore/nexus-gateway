package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// newRouteCmd builds `nexus route explain`, the routing dry-run ("why this
// route"). It fires no real request.
func newRouteCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Inspect routing decisions",
	}
	cmd.AddCommand(newRouteExplainCmd(a))
	return cmd
}

func newRouteExplainCmd(a *App) *cobra.Command {
	var model, endpoint string
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show which provider/model a request resolves to (dry-run)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if model == "" {
				return fmt.Errorf("%w: --model is required", errUsage)
			}
			res, err := a.client().RoutingSimulate(cmd.Context(), core.RoutingSimulateRequest{
				ModelID: model, EndpointType: endpoint,
			})
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(res)
			}
			if res.Substituted {
				a.printf("substituted: yes (rule %q)\n", res.RuleName)
			} else {
				a.printf("substituted: no\n")
			}
			a.printf("\ntargets:\n")
			printTargets(a, res.Targets)
			if len(res.RecoveryTargets) > 0 {
				a.printf("\nrecovery:\n")
				printTargets(a, res.RecoveryTargets)
			}
			if len(res.Warnings) > 0 {
				a.printf("\nwarnings:\n")
				for _, w := range res.Warnings {
					a.printf("  ⚠ %s\n", w)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "model slug to resolve (required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "chat", "endpoint type: chat | responses | messages")
	return cmd
}

func printTargets(a *App, targets []core.RoutingTarget) {
	if len(targets) == 0 {
		a.printf("  (none — request would be rejected by the router)\n")
		return
	}
	for _, t := range targets {
		a.printf("  %s → %s\n", t.ProviderName, t.ModelCode)
	}
}
