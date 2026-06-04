// Package mcp exposes a tool Registry as Model Context Protocol tools over
// stdio, so external agents and partner platforms drive the gateway through the
// same toolset the in-process agent uses. The server has no auth of its own:
// the registry's tools execute as the principal resolved from the configured
// admin credential, through the same admin API + IAM as any other caller. The
// registry is built by internal/capabilities (which decides which tiers to
// expose); this package only bridges it onto the SDK.
package mcp

import (
	"context"
	"encoding/json"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// NewServer builds an MCP server that bridges every tool in reg onto the SDK.
// The tool's JSON schema is passed straight through; its Result content + IsError
// map to the CallToolResult.
func NewServer(reg *agent.Registry) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "nexus", Title: "Nexus Operator Toolkit", Version: "v1"}, nil)
	for _, name := range reg.Names() {
		tool, _ := reg.Get(name)
		s.AddTool(bridge(tool))
	}
	return s
}

// bridge adapts one agent.Tool to an SDK tool definition + handler.
func bridge(t agent.Tool) (*sdk.Tool, sdk.ToolHandler) {
	def := &sdk.Tool{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: json.RawMessage(t.Schema()),
	}
	h := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		res, err := t.Run(ctx, req.Params.Arguments)
		if err != nil {
			// An exceptional wiring error (not a tool-domain failure) surfaces as
			// an MCP protocol error. Tool-domain failures ride Result.IsError.
			return nil, err
		}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: res.Content}},
			IsError: res.IsError,
		}, nil
	}
	return def, h
}

// Serve runs the bridged registry over stdio until the context is cancelled or
// the client disconnects.
func Serve(ctx context.Context, reg *agent.Registry) error {
	return NewServer(reg).Run(ctx, &sdk.StdioTransport{})
}
