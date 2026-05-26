// Smart-routing scenario (S-075) — replaces the previously-skipped
// S-015 placeholder in routing_test.go. Verifies the smart strategy
// end-to-end: admin API accepts the rule, ai-gateway hot-reloads it,
// a chat with model="auto" fires it, and traffic_event records the
// rule plus a concrete routed target (the LLM's pick OR the configured
// fallback default).
//
// Why not "cheapest wins"? The production smart strategy
// (packages/ai-gateway/internal/routing/strategies/strategy_smart.go)
// hands selection to a router-LLM that consumes the candidate catalog
// in its system prompt — there is no deterministic cost-comparator we
// can oracle against. What we CAN assert deterministically:
//
//   1. The admin API accepts strategyType="smart" with a real
//      routerProviderId / routerModelId pair and the operator-side
//      matchConditions guard (E47-S8) requires
//      requestedModelLiterals=["auto"].
//   2. ai-gateway hot-reloads routing_rules (config_applies counter
//      ticks) — proving the rule reached the engine.
//   3. AdminAuditLog stamps the create row.
//   4. A chat with model="auto" + our VK fires the rule:
//      traffic_event.routing_rule_id == <our rule.ID>.
//   5. traffic_event.routed_model_id is non-empty (engine resolved a
//      concrete target — either the LLM's pick OR the fallback default
//      configured on the rule).
//   6. nexus_ai_gateway_requests_total delta ≥ 1 across the chat.
//
// Hermetic: matchConditions pins BOTH virtualKeys=[<this-test's-VK>]
// AND requestedModelLiterals=["auto"] (AND semantics per matcher.go),
// so concurrent scenarios using other VKs / other models cannot pick
// up this rule.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS075_SmartRouting verifies the smart routing strategy end-to-end.
//
// Setup arm — log in, mint a personal VK, look up TWO real moonshot
// models to act as candidates the smart router can pick from (and the
// fallback default if the router LLM is mis-wired). The router LLM
// itself reuses moonshot-v1-8k (the engine just needs a working chat
// credential to issue the decision call — it doesn't need to be one
// of the candidates).
//
// Create-rule arm — POST /api/admin/routing-rules with strategyType
// "smart" and the inline router config. Operator-side guard requires
// matchConditions.requestedModelLiterals=["auto"], so we pin that AND
// virtualKeys=[<vkName>] for hermetic scoping. A non-2xx response is a
// hard failure — the local stack must ship the smart strategy wired.
//
// Request arm — issue /v1/chat/completions with model="auto" via the
// minted VK. Poll traffic_event for our (vk.ID, rule.ID) tuple; assert
// routed_model_id is set (the engine resolved to a real target —
// either the LLM's choice or the fallback default). Metric delta ≥ 1.
func TestS075_SmartRouting(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s075-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// Two real moonshot models — both candidates for the smart router
	// to pick from. The 8k pair also doubles as the router LLM (any
	// working chat credential suffices) and as the fallback default
	// the engine falls back to if the LLM round-trip fails for any
	// reason. Asserting routed_model_id is non-empty is enough to
	// prove either branch fired.
	providerID, model8k, err := helpers.ProviderModelLookup(ctx, sc.Env, token,
		"moonshot", "moonshot-v1-8k")
	if err != nil {
		t.Fatalf("ProviderModelLookup 8k: %v", err)
	}
	_, model32k, err := helpers.ProviderModelLookup(ctx, sc.Env, token,
		"moonshot", "moonshot-v1-32k")
	if err != nil {
		t.Fatalf("ProviderModelLookup 32k: %v", err)
	}

	// Baseline config_applies BEFORE rule create. The
	// `nexus_thingclient_config_applies_total{status="success"}` counter
	// is per-key — each key in a shadow_changed message records its own
	// apply outcome. The previous time_sensitive_patterns wire-shape bug
	// that caused every batch to flip to status=failure was fixed
	// upstream (seed.ts now emits a JSON array, not a JSON-encoded
	// string; AIGw's freshness detector decodes []Rule cleanly — see
	// `freshness detector rules reloaded count=11` in ai-gateway log).
	// We therefore demand a real baseline + WaitForConfigApply success
	// below — no soft-skip fallback.
	preApply, err := helpers.BaselineConfigApply(ctx, sc.Env, "routing_rules")
	if err != nil {
		t.Fatalf("BaselineConfigApply(routing_rules): %v", err)
	}

	// Smart-strategy config is stored flat under config (see
	// strategy_smart.go SmartConfig + types.go StrategyNode smart
	// fields). routerProviderId/routerModelId name the LLM that
	// decides; defaultProviderId/defaultModelId are the fallback
	// when the LLM round-trip fails or returns an unknown model.
	config, _ := json.Marshal(map[string]any{
		"type":              "smart",
		"routerProviderId":  providerID,
		"routerModelId":     model8k,
		"defaultProviderId": providerID,
		"defaultModelId":    model32k,
		// Trim cost / latency on the decision call — the test only
		// needs ONE token of useful output (a model code).
		"maxTokens": 32,
		"timeoutMs": 8000,
	})

	// E47-S8 operator-side guard: smart rules MUST pin
	// requestedModelLiterals=["auto"] — empty / non-auto literals
	// are rejected at admin-API time. We also pin virtualKeys so
	// the rule only fires for our test's VK (concurrent scenarios
	// using "auto" with other VKs remain unaffected).
	match, _ := json.Marshal(map[string]any{
		"virtualKeys":            []string{vkName},
		"requestedModelLiterals": []string{"auto"},
	})

	rule, err := helpers.CreateRoutingRule(ctx, sc.Env, token, helpers.CreateRoutingRuleOpts{
		Name:            "s075-smart-" + vk.ID[:8],
		StrategyType:    "smart",
		Config:          config,
		MatchConditions: match,
		Priority:        100,
	})
	if err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	sc.Cleanup.Register("DeleteRoutingRule("+rule.ID+")", func() error {
		return helpers.DeleteRoutingRule(context.Background(), sc.Env, token, rule.ID)
	})

	// ai-gw must hot-reload routing_rules before the chat fires —
	// otherwise the engine still sees the pre-create config and
	// routes via whatever catch-all rule sits in stage 1. Require a
	// real config_applies counter tick within 15 s — typical Hub→ai-gw
	// push latency on local is <2 s. A failure here means either the
	// hot-reload path is broken or a sibling config_key handler is
	// erroring (would surface as status="failure" instead of
	// "success"); both are real product bugs to investigate, not
	// reasons to silently dwell.
	if _, err := helpers.WaitForConfigApply(ctx, sc.Env, "routing_rules",
		preApply, 15*time.Second); err != nil {
		t.Fatalf("WaitForConfigApply(routing_rules): %v", err)
	}

	// AdminAuditLog: 'create' row stamped with our rule.ID.
	auditRow, err := helpers.WaitForAdminAuditRow(ctx, sc.DB,
		"create", rule.ID, 15*time.Second)
	if err != nil {
		t.Fatalf("WaitForAdminAuditRow: %v", err)
	}
	if auditRow == nil {
		t.Fatalf("AdminAuditLog 'create' row for rule %s did not appear within 15s", rule.ID)
	}

	preMetrics, _ := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)

	// model="auto" is the sentinel the smart strategy looks for
	// (per the matchConditions guard above). The router LLM reads
	// the user message and picks a model from the candidate catalog
	// — the picked model's response comes back to the client.
	body := mustMarshal(t, map[string]any{
		"model": "auto",
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly: HELLO_S075"},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	// Smart routing performs a router-LLM round-trip BEFORE the
	// resolved model call (model="auto" → router LLM picks → engine
	// re-issues against the chosen model). On consumer-grade
	// network paths to moonshot.cn that pair can exceed the
	// shared 30 s client timeout, so use a relaxed 90 s client
	// scoped to this test only.
	client := &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        16,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	status, respBody, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d (body=%q)", status, truncate(respBody, 200))
	}

	// traffic_event row carries routing_rule_id == rule.ID — the
	// smart rule resolved to a concrete target (LLM pick OR fallback
	// default). routed_model_id MUST be non-empty: smart-fallback
	// without a default would return (nil, nil) and the engine would
	// have errored before reaching the upstream, never logging a 200
	// traffic_event.
	predicate := fmt.Sprintf(`source = 'ai-gateway'
		 AND path = '/v1/chat/completions'
		 AND status_code = 200
		 AND identity->'vk'->>'id' = '%s'
		 AND routing_rule_id = '%s'
		 AND routed_model_id IS NOT NULL
		 AND routed_model_id <> ''`, vk.ID, rule.ID)
	row, err := intg.WaitForRecentAuditEvent(
		context.Background(), sc.DB, predicate, nil, 45*time.Second,
	)
	if err != nil {
		t.Fatalf("traffic_event poll failed: %v", err)
	}
	if row == nil {
		t.Fatalf("no traffic_event row matched (rule.ID=%s, vk.ID=%s) — smart rule did not fire or routed_model_id is empty",
			rule.ID, vk.ID)
	}

	// Sanity: routed_model_id must be non-empty (already asserted via
	// the SQL predicate above) and is one of the candidates the engine
	// can legally resolve to. We don't restrict the value to the (8k,
	// 32k) pair — the smart router may legally land on any enabled
	// chat model — but the lookup must succeed.
	var routedModelID string
	if scanErr := sc.DB.QueryRow(context.Background(),
		`SELECT routed_model_id FROM traffic_event WHERE id = $1`, row.ID).Scan(&routedModelID); scanErr != nil {
		t.Fatalf("query routed_model_id: %v", scanErr)
	}
	if routedModelID == "" {
		t.Fatalf("routed_model_id is empty on traffic_event %s — smart rule did not resolve a target", row.ID)
	}

	// Metric delta — the chat MUST leave a counter trace. Catches the
	// silent-failure mode where traffic_event reports 200 but the
	// ingress counter never moved (suggesting the row was stamped
	// from somewhere other than the real request path). The
	// chat/completions request counter is exported as
	// `requests_total{endpoint="chat/completions",status="2xx",...}`.
	postMetrics, _ := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	matchLabels := map[string]string{
		"endpoint": "chat/completions",
		"status":   "2xx",
	}
	reqDelta := postMetrics.CounterSum("requests_total", matchLabels) -
		preMetrics.CounterSum("requests_total", matchLabels)
	if reqDelta < 1 {
		t.Fatalf("requests_total{endpoint=chat/completions,status=2xx} delta=%g, want >= 1 — chat did not exercise gateway", reqDelta)
	}
}
