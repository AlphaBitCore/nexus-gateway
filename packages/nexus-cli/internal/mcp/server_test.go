package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// recordTool is a minimal agent.Tool for bridge tests: it records the raw args
// it was called with and returns a scripted Result (or a Go error).
type recordTool struct {
	name string
	res  agent.Result
	err  error
	got  json.RawMessage
}

func (r *recordTool) Name() string        { return r.name }
func (r *recordTool) Description() string { return r.name + " desc" }
func (r *recordTool) Tier() agent.Tier    { return agent.TierAuto }
func (r *recordTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
}
func (r *recordTool) Run(_ context.Context, in json.RawMessage) (agent.Result, error) {
	r.got = in
	return r.res, r.err
}

func connect(t *testing.T, reg *agent.Registry) (*sdk.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ct, st := sdk.NewInMemoryTransports()
	srv := NewServer(reg)
	go func() { _ = srv.Run(ctx, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, ctx
}

func resultText(res *sdk.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestServerBridgesRegistryTools(t *testing.T) {
	rt := &recordTool{name: "observe_health", res: agent.Result{Content: "healthy"}}
	reg := agent.NewRegistry()
	reg.Register(rt)
	sess, ctx := connect(t, reg)

	// tools/list exposes the registry tool with its schema.
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.Tools) != 1 || lt.Tools[0].Name != "observe_health" || lt.Tools[0].Description != "observe_health desc" {
		t.Fatalf("bridge must list registry tools with metadata, got %+v", lt.Tools)
	}

	// tools/call dispatches the raw args to the agent tool and maps the content.
	out, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "observe_health", Arguments: json.RawMessage(`{"x":"y"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(rt.got) != `{"x":"y"}` {
		t.Fatalf("bridge must pass raw args to the tool, got %s", rt.got)
	}
	if resultText(out) != "healthy" || out.IsError {
		t.Fatalf("bridge must return the tool content, got text=%q isErr=%v", resultText(out), out.IsError)
	}
}

func TestServerMapsErrorResultToIsError(t *testing.T) {
	// A tool-domain failure (Result.IsError) maps to CallToolResult.IsError, NOT
	// an MCP protocol error — so the calling agent can read it and adapt.
	rt := &recordTool{name: "observe_cost", res: agent.Result{Content: "no data", IsError: true}}
	reg := agent.NewRegistry()
	reg.Register(rt)
	sess, ctx := connect(t, reg)
	out, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "observe_cost", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("a tool-domain failure must not be a protocol error: %v", err)
	}
	if !out.IsError || resultText(out) != "no data" {
		t.Fatalf("Result.IsError must map to CallToolResult.IsError, got %+v", out)
	}
}

func TestServerToolGoErrorIsProtocolError(t *testing.T) {
	rt := &recordTool{name: "observe_nodes", err: errors.New("wiring boom")}
	reg := agent.NewRegistry()
	reg.Register(rt)
	sess, ctx := connect(t, reg)
	_, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "observe_nodes", Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "wiring boom") {
		t.Fatalf("a Go error from the tool must surface as an MCP protocol error, got %v", err)
	}
}

func TestServerListsAllRegisteredTools(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(&recordTool{name: "a"})
	reg.Register(&recordTool{name: "b"})
	reg.Register(&recordTool{name: "c"})
	sess, ctx := connect(t, reg)
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.Tools) != 3 {
		t.Fatalf("bridge must list every registry tool, got %d", len(lt.Tools))
	}
}

func TestServeReturnsOnCancelledContext(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(&recordTool{name: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Serve over stdio returns promptly when the context is already cancelled.
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, reg) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on a cancelled context")
	}
}
