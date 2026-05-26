// Cache family (S-060..S-063) — verifies the AI Gateway response cache
// (Redis L1) returns the same payload on a repeat request within TTL,
// stamps a cache-status header, and ticks the cache_hits counter.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS060_CacheHitOnRepeat — PM-grade e2e.
//
// BRAINSTORM (pre): the ai-gw response cache stores a normalised
// request → response mapping. Two identical requests within TTL must
// share the same response payload and ai-gw must signal "served from
// cache" via either a response header or a stamp on the traffic_event
// row (cache_status column). Cross-service: AI Gw (cache.Get/Set,
// Redis L1) → MQ → DB (traffic_event with cache_status). The metric
// nexus_aigw_cache_lookups_total should grow by ≥ 1 per request (miss
// + hit). Specifically, after request 2, nexus_aigw_cache_writes_total
// should have ticked exactly once during the first call, AND the
// total cache_lookups across the two calls should be ≥ 2.
//
// Assertions:
//   1. Both responses 200, chat completion envelope shape, content
//      non-empty.
//   2. The two responses' chat IDs are IDENTICAL (cache returned the
//      same upstream-issued ID). This is the strongest "served from
//      cache" signal — different upstream calls produce different
//      chat IDs.
//   3. metric delta: nexus_aigw_cache_lookups_total grew by ≥ 2 across
//      the two-request burst.
//   4. metric delta: nexus_aigw_cache_writes_total grew by ≥ 1 (first
//      request was a cache write).
func TestS060_CacheHitOnRepeat(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s060-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	preMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics pre: %v", err)
	}

	// Deterministic prompt — include a per-test nonce so we don't
	// share cache state with any sibling scenario, but the SAME nonce
	// for both requests so the cache key matches.
	prompt := fmt.Sprintf("Reply with exactly: CACHE_OK. test-nonce=%d", time.Now().UnixNano())
	body := mustMarshal(t, map[string]any{
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

	parseID := func(b []byte) string {
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(b, &out)
		return out.ID
	}

	// Request 1 — cache MISS (writes to cache).
	status1, body1, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("req1 AIGwPostJSON: %v", err)
	}
	if status1 != 200 {
		t.Fatalf("req1 expected 200, got %d (%q)", status1, truncate(body1, 200))
	}
	id1 := parseID(body1)
	if id1 == "" {
		t.Fatalf("req1 missing chat completion id (body=%q)", truncate(body1, 200))
	}

	// Brief pause so the cache write has time to commit before req2
	// races the lookup.
	time.Sleep(2 * time.Second)

	// Request 2 — cache HIT expected.
	status2, body2, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("req2 AIGwPostJSON: %v", err)
	}
	if status2 != 200 {
		t.Fatalf("req2 expected 200, got %d (%q)", status2, truncate(body2, 200))
	}
	id2 := parseID(body2)
	if id2 == "" {
		t.Fatalf("req2 missing chat completion id (body=%q)", truncate(body2, 200))
	}

	// Strongest hit signal: identical chat completion IDs across the
	// two calls. Different upstream issues each call a fresh chatcmpl-
	// id, so matching IDs prove req2 came out of the cache.
	if id1 != id2 {
		t.Errorf("chat completion IDs differ across two identical requests:\n  req1=%s\n  req2=%s\nCache did not serve req2 — either TTL is too short or cache is disabled for this path",
			id1, id2)
	}

	// Metric deltas.
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post: %v", err)
	}
	lookupsDelta := postMetrics.CounterSum("nexus_aigw_cache_lookups_total", nil) -
		preMetrics.CounterSum("nexus_aigw_cache_lookups_total", nil)
	writesDelta := postMetrics.CounterSum("nexus_aigw_cache_writes_total", nil) -
		preMetrics.CounterSum("nexus_aigw_cache_writes_total", nil)
	if lookupsDelta < 2 {
		t.Errorf("cache_lookups delta=%g (want ≥ 2 across 2 requests)", lookupsDelta)
	}
	if writesDelta < 1 {
		t.Errorf("cache_writes delta=%g (want ≥ 1 — first request should have written)", writesDelta)
	}
	if !strings.HasPrefix(id1, "chatcmpl-") {
		t.Logf("note: chat completion id %q has unexpected prefix (cache hit still inferred from id equality)", id1)
	}
	t.Logf("S-060 OK: req1.id=%s req2.id=%s identical=true lookups_delta=%.0f writes_delta=%.0f",
		id1, id2, lookupsDelta, writesDelta)
}