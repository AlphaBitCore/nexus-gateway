package runtime

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

func TestFuncToolSatisfiesAgentTool(t *testing.T) {
	var called json.RawMessage
	ft := &funcTool{
		name: "observe_cost", desc: "cost by provider", tier: agent.TierAuto,
		schema: json.RawMessage(`{"type":"object","properties":{"groupBy":{"type":"string"}}}`),
		run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
			called = in
			return agent.Result{Content: "ok"}, nil
		},
	}
	var _ agent.Tool = ft // compile-time: it is an agent.Tool
	if ft.Name() != "observe_cost" || ft.Description() != "cost by provider" || ft.Tier() != agent.TierAuto {
		t.Fatalf("metadata wrong: %q %q %v", ft.Name(), ft.Description(), ft.Tier())
	}
	if string(ft.Schema()) == "" {
		t.Fatal("schema must be carried through")
	}
	res, err := ft.Run(context.Background(), json.RawMessage(`{"groupBy":"user"}`))
	if err != nil || res.Content != "ok" || string(called) != `{"groupBy":"user"}` {
		t.Fatalf("Run should invoke the closure with the input, got res=%+v err=%v called=%s", res, err, called)
	}
}

func TestFuncToolNilSchemaDefaultsToObject(t *testing.T) {
	ft := &funcTool{name: "x", run: func(context.Context, json.RawMessage) (agent.Result, error) {
		return agent.Result{}, errors.New("nope")
	}}
	if string(ft.Schema()) != `{"type":"object"}` {
		t.Fatalf("nil schema must default to an empty object schema, got %s", ft.Schema())
	}
}
