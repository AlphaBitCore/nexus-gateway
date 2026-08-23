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
		{"loadbalance weighted", `{"type":"loadbalance","algorithm":"weighted","weightedTargets":[{"weight":5,"node":{"type":"single","providerId":"p","modelId":"m"}}]}`},
		// A payload carrying the sticky fields still validates. They named a
		// session affinity the gateway never had: no code read either one, and
		// the rule form's help text promised Redis-backed stickiness across
		// replicas that did not exist. The fields are gone; refusing the
		// payloads that carry them would break callers for no gain, since the
		// keys did nothing when they were declared either.
		{"loadbalance with the removed sticky keys", `{"type":"loadbalance","algorithm":"weighted","stickyOn":"user","stickyTtlMs":60000,"weightedTargets":[{"weight":5,"node":{"type":"single","providerId":"p","modelId":"m"}}]}`},
		{"conditional with default", `{"type":"conditional","conditions":[{"when":{"requestedModel":"gpt-4o"},"then":{"type":"single","providerId":"p","modelId":"m"}}],"default":{"type":"single","providerId":"p2","modelId":"m2"}}`},
		{"ab_split", `{"type":"ab_split","abTargets":[{"providerId":"p1","modelId":"m1","weight":50},{"providerId":"p2","modelId":"m2","weight":50}]}`},
		{"latency", `{"type":"latency","latencyTargets":[{"providerId":"p1","modelId":"m1"},{"providerId":"p2","modelId":"m2"}]}`},
		{"smart", `{"type":"smart","routerProviderId":"rp","routerModelId":"rm","systemPrompt":"route well","temperature":0.1,"maxTokens":64,"timeoutMs":3000,"defaultProviderId":"dp","defaultModelId":"dm"}`},
		{"empty object (caller enforces required)", `{}`},
	}
	for _, tc := range accept {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if msg, ok := validateStrategyConfig(json.RawMessage(tc.raw)); !ok {
				t.Fatalf("must accept legal config; rejected with %q: %s", msg, tc.raw)
			}
		})
	}

	// Depth used to be the rule: any known node type was accepted up to ten
	// levels, because the gateway evaluated one strategy inside another. It no
	// longer does — a child names a provider and a model, and is resolved as a
	// leaf — so the validator that mirrors it stops at the first level.
	//
	// These two cases replace a pair that asserted an 8-deep tree was legal and
	// a 12-deep one was not. They were not stale about the boundary's job: a
	// validator that accepts what the gateway cannot evaluate persists it,
	// broadcasts it fleet-wide, and then routes nothing. What changed is what
	// the gateway evaluates.
	t.Run("a nested strategy is refused where the admin can be told", func(t *testing.T) {
		msg, ok := validateStrategyConfig(json.RawMessage(
			`{"type":"fallback","targets":[{"type":"loadbalance","weightedTargets":[` +
				`{"weight":1,"node":{"type":"single","providerId":"p","modelId":"m"}}]}]}`))
		if ok {
			t.Fatal("a nested strategy was accepted; it is persisted and broadcast, and then " +
				"resolves to nothing on every gateway while the admin sees an enabled rule")
		}
		if !strings.Contains(msg, "single") {
			t.Errorf("the refusal does not say what a nested entry must be: %q", msg)
		}
		if !strings.Contains(msg, "route nothing") {
			t.Errorf("the refusal does not say what would happen if it were accepted: %q", msg)
		}
	})

	t.Run("a chain of leaves is what the admin surface writes, and stays legal", func(t *testing.T) {
		if msg, ok := validateStrategyConfig(json.RawMessage(
			`{"type":"fallback","targets":[` +
				`{"type":"single","providerId":"p1","modelId":"m1"},` +
				`{"type":"single","providerId":"p2","modelId":"m2"}]}`)); !ok {
			t.Fatalf("the shape the UI writes was rejected: %q", msg)
		}
	})

}

// TestValidateStrategyConfig_PolicyIsRefusedAsANodeToo.
//
// The node validator reads the same closed set as the top-level strategyType,
// so removing `policy` from one has to remove it from the other. It did not,
// once: the top-level check was the one people read, and a `{"type":"policy"}`
// CHILD kept passing — persisted, broadcast fleet-wide, and then resolving to
// no target on every gateway.
//
// A node type the resolver cannot dispatch is refused wherever it appears.
func TestValidateStrategyConfig_PolicyIsRefusedAsANodeToo(t *testing.T) {
	for _, tc := range []struct{ name, cfg string }{
		{"as the whole config", `{"type":"policy","allowModelIds":["m1"]}`},
		{"as a fallback child", `{"type":"fallback","targets":[{"type":"policy","allowModelIds":["m1"]}]}`},
		{"as a conditional branch", `{"type":"conditional","conditions":[{"when":{"requestedModel":"x"},"then":{"type":"policy"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := validateStrategyConfig([]byte(tc.cfg))
			if ok {
				t.Fatalf("accepted %s — it is persisted and broadcast, then resolves to "+
					"nothing on every gateway while the admin sees an enabled rule", tc.cfg)
			}
			if !strings.Contains(msg, "policy") {
				t.Errorf("the refusal does not name the offending type: %q", msg)
			}
		})
	}
}
