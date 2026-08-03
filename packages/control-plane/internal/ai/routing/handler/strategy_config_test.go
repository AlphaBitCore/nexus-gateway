package routing

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// TestValidateStrategyConfig_RecursiveShape pins the CP↔gateway contract the
// old shallow projection missed: the resolver unmarshals the persisted config
// into core.StrategyNode on EVERY request and fails closed on a type
// mismatch, so a nested element with a wrong-typed field that passes the CP
// write gate turns into a per-request outage on that rule. The full mirror
// must therefore reject nested mistypes, unknown nested node types, and
// unreachable depth — while accepting every legitimate strategy tree.
func TestValidateStrategyConfig_RecursiveShape(t *testing.T) {
	reject := []struct {
		name string
		raw  string
	}{
		{"nested weighted target weight as string (the drift bug)",
			`{"type":"loadbalance","algorithm":"weighted","weightedTargets":[{"weight":"5","node":{"type":"single","providerId":"p","modelId":"m"}}]}`},
		{"nested fallback child providerId as number",
			`{"type":"fallback","targets":[{"type":"single","providerId":123,"modelId":"m"}]}`},
		{"nested ab target weight as string",
			`{"type":"ab_split","abTargets":[{"providerId":"p","modelId":"m","weight":"50"}]}`},
		{"nested latency target modelId as object",
			`{"type":"latency","latencyTargets":[{"providerId":"p","modelId":{}}]}`},
		{"nested conditional then with unknown type",
			`{"type":"conditional","conditions":[{"when":{"model":"x"},"then":{"type":"bogus"}}]}`},
		{"nested fallback child with unknown type",
			`{"type":"fallback","targets":[{"type":"nope"}]}`},
		{"onStatusCodes as strings",
			`{"type":"fallback","onStatusCodes":["502"],"targets":[{"type":"single","providerId":"p","modelId":"m"}]}`},
		{"smart temperature as string",
			`{"type":"smart","routerProviderId":"p","routerModelId":"m","temperature":"0.2"}`},
	}
	for _, tc := range reject {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			msg, ok := validateStrategyConfig(json.RawMessage(tc.raw))
			if ok {
				t.Fatalf("must reject; config=%s", tc.raw)
			}
			if msg == "" {
				t.Fatal("rejection must carry an operator-facing message")
			}
		})
	}

	// Golden lockstep samples: one legitimate config per node type, written
	// exactly as the gateway resolver parses them. Every one must pass — a
	// mirror that rejects a legal tree is a fleet-wide config outage of the
	// opposite kind.
	accept := []struct {
		name string
		raw  string
	}{
		{"single", `{"type":"single","providerId":"p","modelId":"m"}`},
		{"fallback with children", `{"type":"fallback","onStatusCodes":[502,429],"targets":[{"type":"single","providerId":"p1","modelId":"m1"},{"type":"single","providerId":"p2","modelId":"m2"}]}`},
		{"loadbalance weighted", `{"type":"loadbalance","algorithm":"weighted","stickyOn":"user","stickyTtlMs":60000,"weightedTargets":[{"weight":5,"node":{"type":"single","providerId":"p","modelId":"m"}}]}`},
		{"conditional with default", `{"type":"conditional","conditions":[{"when":{"requestedModel":"gpt-4o"},"then":{"type":"single","providerId":"p","modelId":"m"}}],"default":{"type":"single","providerId":"p2","modelId":"m2"}}`},
		{"ab_split", `{"type":"ab_split","abTargets":[{"providerId":"p1","modelId":"m1","weight":50},{"providerId":"p2","modelId":"m2","weight":50}]}`},
		{"latency", `{"type":"latency","latencyTargets":[{"providerId":"p1","modelId":"m1"},{"providerId":"p2","modelId":"m2"}]}`},
		{"smart", `{"type":"smart","routerProviderId":"rp","routerModelId":"rm","systemPrompt":"route well","temperature":0.1,"maxTokens":64,"timeoutMs":3000,"defaultProviderId":"dp","defaultModelId":"dm"}`},
		{"policy", `{"type":"policy","allowModelIds":["m1"],"denyProviderIds":["p9"]}`},
		{"empty object (caller enforces required)", `{}`},
	}
	for _, tc := range accept {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if msg, ok := validateStrategyConfig(json.RawMessage(tc.raw)); !ok {
				t.Fatalf("must accept legal config; rejected with %q: %s", msg, tc.raw)
			}
		})
	}

	t.Run("depth beyond the gateway's evaluation limit rejected", func(t *testing.T) {
		// Build a fallback chain 12 levels deep (> maxStrategyNodeDepth).
		inner := `{"type":"single","providerId":"p","modelId":"m"}`
		for range 12 {
			inner = `{"type":"fallback","targets":[` + inner + `]}`
		}
		msg, ok := validateStrategyConfig(json.RawMessage(inner))
		if ok {
			t.Fatal("must reject unreachable depth")
		}
		if !strings.Contains(msg, "depth") {
			t.Fatalf("message should mention depth: %q", msg)
		}
	})

	t.Run("depth within limit accepted", func(t *testing.T) {
		inner := `{"type":"single","providerId":"p","modelId":"m"}`
		for range 8 {
			inner = `{"type":"fallback","targets":[` + inner + `]}`
		}
		if msg, ok := validateStrategyConfig(json.RawMessage(inner)); !ok {
			t.Fatalf("8-deep tree is legal; rejected with %q", msg)
		}
	})
}
