package cli

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// newSimulateCmd runs one crafted request through the real gateway pipeline via
// the admin simulator-forward endpoint (the "request lab"), printing the full
// upstream response. The request is admin-authed; the VK is the upstream
// credential it forwards under.
func newSimulateCmd(a *App) *cobra.Command {
	var model, vk, prompt string
	cmd := &cobra.Command{
		Use:   "simulate",
		Short: "Run a crafted request through the gateway pipeline (request lab)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			modelSlug, err := a.resolveModel(model)
			if err != nil {
				return err
			}
			secret, err := a.vkSecret(vk)
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{
				"model":      modelSlug,
				"messages":   []map[string]string{{"role": "user", "content": prompt}},
				"max_tokens": 64,
			})
			if err != nil {
				return err
			}
			raw, err := a.client().SimulatorForward(cmd.Context(), core.SimulatorForwardRequest{
				Path:   "/v1/chat/completions",
				Method: "POST",
				VK:     secret,
				Body:   body,
			})
			if err != nil {
				return err
			}
			if a.isJSON() {
				_, err := a.Out.Write(append(raw, '\n'))
				return err
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, raw, "", "  "); err != nil {
				a.printf("%s\n", raw) // not JSON (e.g. an error envelope) — print raw
				return nil
			}
			fmt.Fprintln(a.Out, pretty.String())
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "model slug (default: remembered selection)")
	cmd.Flags().StringVar(&vk, "vk", "", "Virtual Key secret (default: stored by the TUI wizard)")
	cmd.Flags().StringVar(&prompt, "prompt", "hello", "prompt to send through the pipeline")
	return cmd
}
