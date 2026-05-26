// FreshMark family (S-081) — verifies the E61-S1 time-sensitive
// freshness gate skips both cache tiers when an admin-configured pattern
// matches the request prompt.
//
// BRAINSTORM (pre): the AI Gateway's response cache has two tiers
// (L1 extract / L2 semantic). For queries whose answer must be fresh
// (stock prices, weather, "now/today" intent), serving a cached reply
// would leak stale content. E61-S1 introduces a fleet-wide rule list
// — `semantic_cache_config.time_sensitive_overrides` JSONB — managed via
// `/api/admin/cache/time-sensitive-patterns`. Each rule carries a
// keyword list (case-insensitive substring match), optional
// requireQuestionMark / requireEntity heuristics, languages, and an
// enabled flag. When the freshness detector matches the canonical user
// prompt AND the L1 `apply_freshness_rules` gate is on, the gateway
// stamps `traffic_event.gateway_cache_skip_reason = 'time_sensitive'`
// and short-circuits cache lookup + write on BOTH tiers.
//
// Cross-service path: Admin UI → CP admin
// (`packages/control-plane/internal/ai/cache/handler/time_sensitive.go`,
// `RegisterTimeSensitiveRoutes`) → DB `semantic_cache_config.time_sensitive_overrides`
// + Hub shadow `response_cache.time_sensitive_patterns` → AI Gw
// `freshness.Detector` →
// `packages/ai-gateway/internal/ingress/proxy/proxy.go::classifyCachePreLookup`
// → `audit.GatewayCacheSkipReasonTimeSensitive` (value `"time_sensitive"`,
// see `packages/ai-gateway/internal/platform/audit/audit.go`).
//
// Arms (3):
//  1. Read original rule list — GET `/api/admin/cache/time-sensitive-patterns`.
//     Capture for restore (the seed populates 11 default rules; we must
//     not leak our test rule into the fleet).
//  2. Add a uniquely-keyed rule — POST with a marker keyword that cannot
//     accidentally match any seeded rule. Register a DELETE cleanup that
//     runs regardless of test outcome.
//  3. Verify the rule fires end-to-end:
//      (a) dry-run POST `/test` with a prompt containing the marker →
//          assert `decision == "match"` and `matchedRuleId == <our id>`.
//      (b) live POST `/v1/chat/completions` with the same marker →
//          assert 200, then poll `traffic_event` until a row with
//          `gateway_cache_skip_reason = 'time_sensitive'` shows up for
//          our VK.
//
// Skip-graceful: if the freshness-rules endpoint 404s (E61-S6 T5e not
// deployed at this target) we `t.Skipf` rather than fail.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS081_FreshnessRuleSkipsCache — PM-grade e2e for the E61-S1
// fleet-wide time-sensitive freshness gate.
func TestS081_FreshnessRuleSkipsCache(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s081-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// ---------------- Arm 1: read original rule list ----------------
	//
	// The catalog binding is "tests must only touch own data" — we don't
	// mutate the seeded defaults, but we DO insert one new rule, so we
	// snapshot the list for diagnostics (cleanup only needs to DELETE
	// our own rule by id, registered after Arm 2 succeeds).
	getStatus, getBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"GET", "/api/admin/cache/time-sensitive-patterns", nil)
	if err != nil {
		t.Fatalf("GET time-sensitive-patterns: %v", err)
	}
	if getStatus == 404 {
		t.Fatalf("S-081 precondition unmet: GET /api/admin/cache/time-sensitive-patterns returned 404. E61-S6 T5e freshness-rules admin endpoints must be deployed (RegisterTimeSensitiveRoutes wired + CP rebuilt) before this scenario can run (body=%q)",
			truncate(getBody, 200))
	}
	if getStatus != 200 {
		t.Fatalf("GET time-sensitive-patterns expected 200, got %d (%q)",
			getStatus, truncate(getBody, 300))
	}
	var listResp struct {
		Patterns []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"patterns"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(getBody, &listResp); err != nil {
		t.Fatalf("decode patterns list: %v (body=%q)", err, truncate(getBody, 400))
	}
	t.Logf("S-081 pre-state: %d existing freshness rules (source=%q)",
		len(listResp.Patterns), listResp.Source)

	// ---------------- Arm 2: add a uniquely-keyed test rule ----------------
	//
	// The marker is unique per-run so the dry-run / live arms can be
	// certain ONLY our rule could fire — no risk of a seeded "now / today /
	// stock price" keyword swallowing the match and producing a confusing
	// pass. We also force the rule id to a deterministic prefix so the
	// DELETE cleanup is targeted regardless of test outcome.
	marker := fmt.Sprintf("s081marker%d", time.Now().UnixNano())
	ruleID := fmt.Sprintf("s081-test-%d", time.Now().UnixNano())
	rule := map[string]any{
		"id":                  ruleID,
		"keywords":            []string{marker},
		"requireQuestionMark": false,
		"requireEntity":       false,
		"languages":           []string{"en"},
		"enabled":             true,
	}
	postBody := mustMarshal(t, rule)
	postStatus, postRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"POST", "/api/admin/cache/time-sensitive-patterns", postBody)
	if err != nil {
		t.Fatalf("POST time-sensitive-patterns: %v", err)
	}
	if postStatus == 404 {
		t.Fatalf("S-081 precondition unmet: POST /api/admin/cache/time-sensitive-patterns returned 404 (Arm 1 GET succeeded, so this is asymmetric routing). E61-S6 T5e POST route must be wired before this scenario can run (body=%q)",
			truncate(postRespBody, 200))
	}
	if postStatus != 201 {
		t.Fatalf("POST time-sensitive-patterns expected 201, got %d (%q)",
			postStatus, truncate(postRespBody, 300))
	}
	// Register DELETE cleanup IMMEDIATELY so a later assertion failure
	// can't leak our rule into the fleet.
	sc.Cleanup.Register("DeleteFreshnessRule("+ruleID+")", func() error {
		status, body, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			"DELETE", "/api/admin/cache/time-sensitive-patterns/"+ruleID, nil)
		if err != nil {
			return fmt.Errorf("DELETE freshness rule: %w", err)
		}
		// 200 = removed, 404 = already gone (idempotent). Anything else
		// is a real failure worth surfacing.
		if status != 200 && status != 404 {
			return fmt.Errorf("DELETE freshness rule status %d body=%q",
				status, truncate(body, 200))
		}
		return nil
	})

	// ---------------- Arm 2.5: flip the L1 apply_freshness_rules gate ON ----------------
	//
	// classifyCachePreLookup short-circuits the freshness branch when the
	// extract-cache gate is false — even with a perfectly-matching rule,
	// no skip stamp lands on traffic_event and L1/L2 cache proceeds. The
	// gate is fleet-wide singleton; we snapshot the current config,
	// register a restore-on-exit cleanup so we don't leak this change,
	// then PUT enabled+gate=on.
	getCfgStatus, getCfgBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"GET", "/api/admin/extract-cache/config", nil)
	if err != nil {
		t.Fatalf("GET extract-cache/config: %v", err)
	}
	if getCfgStatus != 200 {
		t.Fatalf("GET extract-cache/config expected 200, got %d (%q)",
			getCfgStatus, truncate(getCfgBody, 300))
	}
	var origCfg struct {
		Enabled             bool `json:"enabled"`
		TTLSeconds          int  `json:"ttlSeconds"`
		ApplyFreshnessRules bool `json:"applyFreshnessRules"`
	}
	if err := json.Unmarshal(getCfgBody, &origCfg); err != nil {
		t.Fatalf("decode extract-cache/config: %v (body=%q)", err, truncate(getCfgBody, 400))
	}
	// Register restore IMMEDIATELY so a later assertion failure cannot
	// leak the gate flip into the fleet.
	sc.Cleanup.Register("RestoreExtractCacheConfig", func() error {
		// PutConfig requires ttlSeconds in [60, 604800] when enabled=true.
		// If the seed left ttl=0 with enabled=false, send what we got;
		// otherwise restore exactly.
		restoreBody, _ := json.Marshal(map[string]any{
			"enabled":             origCfg.Enabled,
			"ttlSeconds":          origCfg.TTLSeconds,
			"applyFreshnessRules": origCfg.ApplyFreshnessRules,
		})
		status, body, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			"PUT", "/api/admin/extract-cache/config", restoreBody)
		if err != nil {
			return fmt.Errorf("restore extract-cache/config: %w", err)
		}
		if status != 200 {
			return fmt.Errorf("restore extract-cache/config status %d body=%q",
				status, truncate(body, 200))
		}
		return nil
	})
	// Force the gate ON. Preserve whatever ttl was set; if the seed left
	// ttl=0 (i.e., enabled=false default), pick a safe 3600 — the
	// freshness gate is what matters here, not cache TTL.
	wantTTL := origCfg.TTLSeconds
	if wantTTL < 60 {
		wantTTL = 3600
	}
	putCfgBody := mustMarshal(t, map[string]any{
		"enabled":             true,
		"ttlSeconds":          wantTTL,
		"applyFreshnessRules": true,
	})
	putCfgStatus, putCfgRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"PUT", "/api/admin/extract-cache/config", putCfgBody)
	if err != nil {
		t.Fatalf("PUT extract-cache/config: %v", err)
	}
	if putCfgStatus != 200 {
		t.Fatalf("PUT extract-cache/config expected 200, got %d (%q)",
			putCfgStatus, truncate(putCfgRespBody, 300))
	}
	// The PUT pushes the new state to Hub fire-and-forget; the AIGw
	// atomic swap happens on receipt. Allow a brief window for the
	// shadow push to land before driving live traffic.
	time.Sleep(1 * time.Second)

	// ---------------- Arm 3a: dry-run match via /test ----------------
	//
	// The /test endpoint runs detectPrompt on the merged rule list
	// without touching the cache path — fastest possible signal that the
	// rule was persisted AND propagated to the loader. If the dry-run
	// doesn't match, the live arm cannot match either, so fail here with
	// a clear diagnostic rather than waiting on a traffic_event poll.
	//
	// detectPrompt iterates rules in stored order and returns on FIRST
	// match — new rules are append-ed (POST handler line 175), so our
	// rule is LAST. We MUST therefore craft a prompt that contains ONLY
	// our unique marker and avoids every keyword from the 11 seeded rules
	// (time-current's "now"/"today"/"latest"/"current" being the canonical
	// trap). Using a sparse non-natural-language phrase keeps the prompt
	// readable while guaranteeing no seeded keyword can fire first.
	testReqBody := mustMarshal(t, map[string]any{
		"prompt":   fmt.Sprintf("Please describe the entity %s in detail", marker),
		"language": "en",
	})
	testStatus, testRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"POST", "/api/admin/cache/time-sensitive-patterns/test", testReqBody)
	if err != nil {
		t.Fatalf("POST time-sensitive-patterns/test: %v", err)
	}
	if testStatus != 200 {
		t.Fatalf("POST /test expected 200, got %d (%q)",
			testStatus, truncate(testRespBody, 300))
	}
	var testResp struct {
		Decision        string   `json:"decision"`
		MatchedRuleID   *string  `json:"matchedRuleId"`
		MatchedKeywords []string `json:"matchedKeywords"`
	}
	if err := json.Unmarshal(testRespBody, &testResp); err != nil {
		t.Fatalf("decode /test response: %v (body=%q)", err, truncate(testRespBody, 400))
	}
	if testResp.Decision != "match" {
		t.Fatalf("/test decision=%q want \"match\" (body=%q) — rule not propagated to handler's loader",
			testResp.Decision, truncate(testRespBody, 300))
	}
	if testResp.MatchedRuleID == nil || *testResp.MatchedRuleID != ruleID {
		got := "<nil>"
		if testResp.MatchedRuleID != nil {
			got = *testResp.MatchedRuleID
		}
		// The prompt above contains ONLY our unique marker and no
		// seeded-rule keyword (see Arm 3a comment). If another rule
		// still fires first, either (a) a new seed rule was added with
		// a keyword that collides with our sparse phrasing, or (b) the
		// detector's iteration order is broken. Both are regressions
		// worth surfacing — not skip-able.
		t.Fatalf("/test matchedRuleId=%q want %q — a seeded freshness rule beat our marker-only prompt (%d pre-existing rules). Check tools/db-migrate/seed/data/time-sensitive-rules.json for a newly-added keyword that collides with the Arm 3a prompt template.",
			got, ruleID, len(listResp.Patterns))
	}

	// ---------------- Arm 3b: live request → traffic_event skip stamp ----------------
	//
	// Now drive a real chat completion containing the marker. The
	// gateway's freshness detector should match our rule, classify the
	// request as time-sensitive, and stamp gateway_cache_skip_reason on
	// the resulting traffic_event row.
	// Same sparse-phrasing rule as Arm 3a — avoid every seeded keyword;
	// the nonce is digits only so it cannot collide with any keyword
	// (all seeded keywords contain at least one letter).
	prompt := fmt.Sprintf("Please describe the entity %s in detail. nonce=%d", marker, time.Now().UnixNano())
	chatBody := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()
	status, respBody, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", chatBody)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	// classifyCachePreLookup stamps gateway_cache_skip_reason BEFORE the
	// upstream call, so the traffic_event row carries our assertion
	// signal whether or not the upstream returns 200. A transient
	// upstream failure (TLS handshake timeout, rate-limit, 5xx) still
	// produces a traffic_event row with skip_reason='time_sensitive' —
	// which is exactly what S-081 is validating. We log the status for
	// diagnostics but don't gate on 200; the assertion is the DB row.
	t.Logf("Arm 3b: /v1/chat/completions status=%d (any status acceptable; assertion is the traffic_event row stamp). body=%q",
		status, truncate(respBody, 200))

	// Poll traffic_event for our VK's row. Audit pipeline latency is the
	// usual gateway → MQ → Hub consumer → DB ~10 s typical, 30 s worst —
	// use the 45 s deadline standard across this suite.
	//
	// IMPORTANT: assert gateway_cache_skip_reason == 'time_sensitive'.
	// The L1 `apply_freshness_rules` gate must also be on for the stamp
	// to fire — when the gate is off (default for fresh installs that
	// haven't touched extract-cache/config), the detector still matches
	// but classifyCachePreLookup returns ("", ""), no skip is stamped,
	// and L1/L2 cache proceeds normally. In that case the row's
	// skip-reason column is NULL/empty, and we t.Skipf with a clear
	// diagnostic so a maintainer can flip the gate and re-run rather
	// than chase a phantom regression.
	predicate := fmt.Sprintf(`source = 'ai-gateway'
		 AND path = '/v1/chat/completions'
		 AND identity->'vk'->>'id' = '%s'`, vk.ID)
	row, err := intg.WaitForRecentAuditEvent(
		context.Background(), sc.DB, predicate, nil, 45*time.Second,
	)
	if err != nil {
		t.Fatalf("traffic_event poll: %v", err)
	}
	if row == nil {
		t.Fatalf("no traffic_event row for VK %s within 45 s — audit pipeline stuck or VK lookup failed", vk.ID)
	}

	// Read the cache columns directly — WaitForRecentAuditEvent's
	// AuditEventRow type doesn't expose them, so we re-query by id.
	var skipReason, cacheStatus *string
	queryErr := sc.DB.QueryRow(ctx, `
		SELECT gateway_cache_skip_reason, gateway_cache_status
		FROM traffic_event
		WHERE id = $1
	`, row.ID).Scan(&skipReason, &cacheStatus)
	if queryErr != nil {
		t.Fatalf("read cache columns for traffic_event %s: %v", row.ID, queryErr)
	}

	if skipReason == nil || *skipReason == "" {
		// Arm 2.5 already flipped the L1 apply_freshness_rules gate ON
		// (and Arm 3a confirmed the rule matches via /test). If the
		// live path still didn't stamp the skip column, the breakage is
		// in classifyCachePreLookup → freshness.Detector → Hub-shadow
		// propagation — a real regression in the E61-S1 live path, not
		// an env precondition. Fail hard so the maintainer can chase
		// the actual break.
		gotCacheStatus := "<nil>"
		if cacheStatus != nil {
			gotCacheStatus = *cacheStatus
		}
		t.Fatalf("traffic_event.gateway_cache_skip_reason is nil/empty after gate ON + rule matched at /test (traffic_event.id=%s, cache_status=%q). Live freshness path is broken — check ai-gateway classifyCachePreLookup + Hub shadow propagation of ResponseCacheTimeSensitivePatterns.",
			row.ID, gotCacheStatus)
	}
	if *skipReason != "time_sensitive" {
		t.Errorf("traffic_event.gateway_cache_skip_reason=%q want %q (traffic_event.id=%s) — wrong skip path engaged",
			*skipReason, "time_sensitive", row.ID)
	}
	t.Logf("S-081 OK: rule %s with keyword %q matched live request; traffic_event %s stamped gateway_cache_skip_reason=time_sensitive",
		ruleID, marker, row.ID)
}
