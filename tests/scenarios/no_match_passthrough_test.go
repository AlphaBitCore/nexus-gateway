// No-match passthrough fallback family (S-080) — verifies the AI
// Gateway's E31-S3 behavior: when the routing pipeline resolves zero
// targets for an incoming request, the gateway falls through to a
// passthrough path that looks up the requested model directly via the
// VK-allowed providers + credentials. The traffic_event row carries
// the literal sentinel rule ID "passthrough-fallback".
//
// Mechanism (verified in
// packages/ai-gateway/internal/ingress/proxy/proxy.go:1205
// resolveNoMatchPassthrough): when len(routeResult.Targets)==0 the
// handler invokes resolveNoMatchPassthrough; on success it stamps
// rec.RoutingRuleID = "passthrough-fallback" (a literal string, not a
// UUID) and rec.RoutingTrace.targets[0].source = "passthrough-fallback".
//
// OpenAPI: docs/users/api/openapi/ai-gateway/e31-s3-routing-no-match-passthrough-fallback.yaml
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS080_NoMatchPassthroughFallback — PM-grade e2e for the
// no-match passthrough fallback path (E31-S3).
//
// Setup is hermetic: a fresh personal VK whose Name does not match any
// existing rule's virtualKeys glob ensures only catch-all stage-1 rules
// could fire. The scenario inspects the current routing-rules set; any
// enabled stage-1 rule whose matchConditions would still match our
// request (empty/null/{} matchConditions, or a Models list that includes
// our requested model, or a virtualKeys glob "*") is temporarily
// disabled and re-enabled on cleanup. Rule shapes we don't recognize
// fall through to "leave alone" — the matcher's own fail-closed logic
// (matcher.go:311) ensures they don't match either.
//
// Assertions (all hard — no skips):
//  1. POST /v1/chat/completions returns HTTP 200. A non-200 means
//     passthrough is disabled or the dev credential is broken — both
//     are real environment defects the test surfaces.
//  2. traffic_event row appears within 45s with our VK and
//     routing_rule_id = "passthrough-fallback".
//  3. routing_trace JSONB carries targets[0].source = "passthrough-fallback"
//     so operators inspecting the routing simulator panel see the
//     fallback path explicitly (rather than an empty trace).
func TestS080_NoMatchPassthroughFallback(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s080-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// We deliberately do NOT lookup providerID/modelID here: the
	// passthrough path resolves the model by code itself
	// (proxy.go:1215 ModelsLookup.GetModelByCode). All we need is a
	// catalog model code that exists and is reachable from the dev
	// credentials — moonshot-v1-8k is the standard scenario fixture.
	const testModel = "moonshot-v1-8k"

	// --- Detect existing routing rules that would still match our
	// request despite the fresh VK. The matcher (matcher.go:311
	// RuleMatchesContext) treats a nil/empty MatchConditions as a
	// catch-all: every model matches. Explicit Models lists matching
	// our code, or a virtualKeys glob "*", also match.
	listStatus, listBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/routing-rules?enabled=true", nil)
	if err != nil {
		t.Fatalf("list routing-rules: %v", err)
	}
	if listStatus != http.StatusOK {
		t.Fatalf("list routing-rules: status %d body=%q", listStatus, truncate(listBody, 200))
	}
	var listResp struct {
		Data []struct {
			ID              string          `json:"id"`
			Name            string          `json:"name"`
			StrategyType    string          `json:"strategyType"`
			PipelineStage   int             `json:"pipelineStage"`
			Enabled         bool            `json:"enabled"`
			MatchConditions json.RawMessage `json:"matchConditions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listBody, &listResp); err != nil {
		t.Fatalf("decode list routing-rules: %v (body=%q)", err, truncate(listBody, 200))
	}

	toggled := 0
	for _, r := range listResp.Data {
		// Only stage-1 (routing) rules can produce targets via the
		// primary path. Stage-0 narrowing rules subtract from the
		// candidate set and would actually HELP empty the candidates,
		// so leaving them alone is safe for this scenario.
		if r.PipelineStage != 1 {
			continue
		}
		if !ruleWouldMatchTestRequest(r.MatchConditions, testModel) {
			continue
		}
		// Disable this rule and register a cleanup to re-enable it.
		// We capture by value so the closure can't be invalidated by
		// the loop variable.
		ruleID := r.ID
		ruleName := r.Name
		putBody, _ := json.Marshal(map[string]any{"enabled": false})
		status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
			http.MethodPut, "/api/admin/routing-rules/"+ruleID, putBody)
		if err != nil {
			t.Fatalf("disable routing rule %s (%s): %v", ruleID, ruleName, err)
		}
		if status != http.StatusOK {
			t.Fatalf("disable routing rule %s (%s): status %d body=%q",
				ruleID, ruleName, status, truncate(respBody, 200))
		}
		sc.Cleanup.Register("ReEnableRoutingRule("+ruleID+")", func() error {
			reBody, _ := json.Marshal(map[string]any{"enabled": true})
			_, _, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
				http.MethodPut, "/api/admin/routing-rules/"+ruleID, reBody)
			return err
		})
		toggled++
	}

	// Bound the wait so a sleeper ai-gw doesn't make us hang. If no
	// rule was toggled (clean dev DB), skip the wait entirely.
	if toggled > 0 {
		preApply, err := helpers.BaselineConfigApply(ctx, sc.Env, "routing_rules")
		if err != nil {
			t.Fatalf("BaselineConfigApply: %v", err)
		}
		// We've already issued the PUTs; the hot-reload must land
		// within 30s. If the apply counter doesn't tick, the engine
		// is still reading the pre-toggle ruleset and the fallback
		// path won't fire — that's a real product bug, not a soft
		// warning to dwell on.
		if _, err := helpers.WaitForConfigApply(ctx, sc.Env, "routing_rules",
			preApply, 30*time.Second); err != nil {
			t.Fatalf("WaitForConfigApply(routing_rules): %v", err)
		}
	}

	// --- Issue the chat request. The personal VK has no project, no
	// glob matches against any remaining rule, and (post-toggle) no
	// catch-all rule that would match it — so the resolver returns
	// zero targets and the gateway falls through to
	// resolveNoMatchPassthrough.
	reqBody := mustMarshal(t, map[string]any{
		"model": testModel,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly: HELLO_S080"},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()
	status, respBody, err := intg.AIGwPostJSON(&envForCall, client,
		"/v1/chat/completions", reqBody)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	if status != http.StatusOK {
		// Non-200 on the catch-all-cleared path means the
		// passthrough fallback isn't firing for a real reason:
		// E48 kill-switch, broken dev credential, or unseeded
		// model code. Each is a real environment defect we want
		// surfaced — not silently skipped.
		t.Fatalf("expected HTTP 200 on no-match passthrough, got %d (body=%q)",
			status, truncate(respBody, 200))
	}

	// --- DB assertion: traffic_event row stamped with the literal
	// sentinel rule ID. Match by VK ID so we can't collide with any
	// concurrent scenario's traffic.
	predicate := fmt.Sprintf(`source = 'ai-gateway'
		 AND path = '/v1/chat/completions'
		 AND status_code = 200
		 AND identity->'vk'->>'id' = '%s'
		 AND routing_rule_id = 'passthrough-fallback'`, vk.ID)
	row, err := intg.WaitForRecentAuditEvent(
		context.Background(), sc.DB, predicate, nil, 45*time.Second,
	)
	if err != nil {
		t.Fatalf("traffic_event poll: %v", err)
	}
	if row == nil {
		// Diagnostic: did ANY row appear for this VK? If yes the
		// fallback didn't fire (some other rule still matched);
		// surface the actually-observed routing_rule_id so the
		// follow-up fix can target the right rule.
		altPred := fmt.Sprintf(`source = 'ai-gateway'
			 AND path = '/v1/chat/completions'
			 AND identity->'vk'->>'id' = '%s'`, vk.ID)
		alt, _ := intg.WaitForRecentAuditEvent(
			context.Background(), sc.DB, altPred, nil, 5*time.Second,
		)
		if alt != nil {
			var altRuleID *string
			_ = sc.DB.QueryRow(context.Background(),
				`SELECT routing_rule_id FROM traffic_event WHERE id = $1`,
				alt.ID).Scan(&altRuleID)
			observed := "<null>"
			if altRuleID != nil {
				observed = *altRuleID
			}
			t.Fatalf("chat returned 200 but routing_rule_id was %q, want 'passthrough-fallback' — a residual matching rule still fired", observed)
		}
		t.Fatalf("no traffic_event row appeared within 45s for vk=%s — gateway audit pipeline silent (passthrough marker not observable)", vk.ID)
	}

	// --- routing_trace JSONB cross-check: the buildRoutingAuditTrace
	// helper emits targets[0].source = "passthrough-fallback" on the
	// fallback path. This is the second, narrower marker the UI's
	// traffic detail drawer renders, so verifying it locks both
	// signals against schema drift in either column.
	var trace string
	scanErr := sc.DB.QueryRow(context.Background(),
		`SELECT COALESCE(routing_trace::text, '') FROM traffic_event WHERE id = $1`,
		row.ID).Scan(&trace)
	if scanErr != nil {
		t.Fatalf("query routing_trace: %v", scanErr)
	}
	if trace == "" || trace == "null" || trace == "[]" || trace == "{}" {
		t.Errorf("routing_trace is empty/null on passthrough-fallback row (got %q) — UI trace drawer would render blank",
			trace)
	} else {
		// Soft assertion: look for the source marker. We don't
		// strict-parse because the trace shape evolves
		// (routing_audit_trace.go); the marker substring is the
		// load-bearing invariant.
		var parsed struct {
			Targets []struct {
				Source string `json:"source"`
			} `json:"targets"`
		}
		if err := json.Unmarshal([]byte(trace), &parsed); err != nil {
			t.Errorf("routing_trace parse failed (%v) — raw=%s", err, truncate([]byte(trace), 200))
		} else if len(parsed.Targets) == 0 {
			t.Errorf("routing_trace.targets is empty — buildRoutingAuditTrace would skip the source marker (raw=%s)",
				truncate([]byte(trace), 200))
		} else if parsed.Targets[0].Source != "passthrough-fallback" {
			t.Errorf("routing_trace.targets[0].source=%q, want %q (raw=%s)",
				parsed.Targets[0].Source, "passthrough-fallback",
				truncate([]byte(trace), 200))
		}
	}

}

// ruleWouldMatchTestRequest returns true if the rule's matchConditions
// would still match a fresh personal VK (whose Name doesn't match any
// glob and whose ProjectID is empty) for the given model code. This
// mirrors the matcher's logic in matcher.go:311 RuleMatchesContext:
//
//   - nil / empty / "{}" matchConditions → catch-all, always matches
//   - Models / RequestedModelLiterals containing our model → matches
//   - VirtualKeys glob "*" → matches every VK name (so still matches)
//
// Conservative: rules with non-empty Projects / VirtualKeys that don't
// include "*" / Providers do NOT match our personal-VK request, so we
// leave them alone. Same for Models lists that don't name our code.
func ruleWouldMatchTestRequest(rawConds json.RawMessage, modelCode string) bool {
	// nil RawMessage or JSON `null` → catch-all.
	if len(rawConds) == 0 {
		return true
	}
	trimmed := trimSpace(string(rawConds))
	if trimmed == "" || trimmed == "null" {
		return true
	}
	if trimmed == "{}" {
		return true
	}
	var conds struct {
		Models                 []string `json:"models"`
		RequestedModelLiterals []string `json:"requestedModelLiterals"`
		Providers              []string `json:"providers"`
		ModelTypes             []string `json:"modelTypes"`
		Projects               []string `json:"projects"`
		VirtualKeys            []string `json:"virtualKeys"`
	}
	if err := json.Unmarshal(rawConds, &conds); err != nil {
		// Malformed — the matcher fail-closes (returns false) for
		// such rules. So this rule won't match our request, leave it.
		return false
	}
	// All-empty struct = catch-all (matches every model). Empty
	// VirtualKeys + Projects + Models + Providers + Literals +
	// ModelTypes means RuleMatchesContext falls through every gate
	// and returns true.
	if len(conds.Models) == 0 && len(conds.RequestedModelLiterals) == 0 &&
		len(conds.Providers) == 0 && len(conds.ModelTypes) == 0 &&
		len(conds.Projects) == 0 && len(conds.VirtualKeys) == 0 {
		return true
	}
	// Explicit Models / RequestedModelLiterals list containing our
	// code → would match (Models is checked against CandidateIDs
	// which includes the requested model code).
	for _, m := range conds.Models {
		if m == modelCode {
			return true
		}
	}
	for _, lit := range conds.RequestedModelLiterals {
		if lit == modelCode {
			return true
		}
	}
	// VirtualKeys = ["*"] → matches every VK name, including ours.
	for _, pat := range conds.VirtualKeys {
		if pat == "*" {
			return true
		}
	}
	// Any other shape (Projects=[...], Providers=[...],
	// VirtualKeys=["foo-*"], Models=[<other-code>]) is restrictive
	// enough that the matcher returns false for our fresh personal
	// VK / single requested model code. Leave the rule alone.
	return false
}

// trimSpace is a local helper so this file stays import-light
// (strings.TrimSpace would pull strings into a file that otherwise
// uses only net/http + encoding/json + fmt + time). Mirrors
// strings.TrimSpace for ASCII whitespace, which is sufficient for
// JSON.RawMessage comparisons.
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end {
		c := s[start]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		start++
	}
	for end > start {
		c := s[end-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		end--
	}
	return s[start:end]
}
