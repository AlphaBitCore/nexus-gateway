package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// newChatCmd sends one prompt to the selected model over the AI Gateway
// (VK-authed SSE) and streams the reply. In --output json it collects the full
// reply + usage into a single object so scripts get a stable shape.
func newChatCmd(a *App) *cobra.Command {
	var model, vk string
	cmd := &cobra.Command{
		Use:   "chat [message]",
		Short: "Send a prompt to a model (VK-authed, streamed)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := strings.TrimSpace(strings.Join(args, " "))
			if prompt == "" {
				return fmt.Errorf("%w: provide a message to send", errUsage)
			}
			modelSlug, err := a.resolveModel(model)
			if err != nil {
				return err
			}
			secret, err := a.vkSecret(vk)
			if err != nil {
				return err
			}
			req := core.ChatRequest{
				Model:    modelSlug,
				Messages: []core.ChatMessage{{Role: "user", Content: prompt}},
			}
			var sb strings.Builder
			onDelta := func(d string) {
				sb.WriteString(d)
				if !a.isJSON() {
					fmt.Fprint(a.Out, d) // stream live in table mode
				}
			}
			usage, err := a.client().ChatStream(cmd.Context(), secret, req, onDelta)
			if err != nil {
				return err
			}
			if a.isJSON() {
				return a.renderJSON(map[string]any{"model": modelSlug, "content": sb.String(), "usage": usage})
			}
			a.printf("\n")
			if usage != nil {
				a.printf("— tokens %d (prompt %d, completion %d, cached %d)\n",
					usage.TotalTokens, usage.PromptTokens, usage.CompletionTokens, usage.PromptTokensDetails.CachedTokens)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "model slug (default: remembered selection)")
	cmd.Flags().StringVar(&vk, "vk", "", "Virtual Key secret (default: stored by the TUI wizard)")
	return cmd
}
