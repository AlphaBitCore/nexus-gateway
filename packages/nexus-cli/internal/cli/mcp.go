package cli

import (
	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/mcp"
)

// newMCPCmd builds `nexus mcp serve`, the stdio MCP server.
func newMCPCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol server for agents and partner platforms",
	}
	cmd.AddCommand(newMCPServeCmd(a))
	return cmd
}

func newMCPServeCmd(a *App) *cobra.Command {
	var enableMitigate bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Expose the gateway as MCP tools over stdio",
		Long: "Serves the toolkit's capabilities as MCP tools over stdio. The server has no " +
			"auth of its own: every tool runs as the principal resolved from the configured " +
			"admin credential, through the same admin API + IAM as any other caller. The " +
			"observe / analyze / simulate tiers are always exposed; the mitigate (write) tier " +
			"is off unless --enable-mitigate is set.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The simulate tool forwards under the VK stored for this env, if any.
			vkSecret, _ := a.Store.Get(a.Env.Name, core.SecretVKSecret)
			return mcp.Serve(cmd.Context(), a.client(), mcp.Options{
				EnableMitigate: enableMitigate,
				VKSecret:       vkSecret,
			})
		},
	}
	cmd.Flags().BoolVar(&enableMitigate, "enable-mitigate", false, "expose write (mitigate) tools such as the kill switch (off by default)")
	return cmd
}
