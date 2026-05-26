// Cache family — S-066 negative-feedback eviction (E68/E86 gap closure).
// Verifies the admin cache-feedback endpoint poisons a cached entry so a
// would-be HIT turns into a MISS on the next semantically-similar request.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS066_CacheNegativeFeedback — PM-grade e2e for E68 negative-feedback.
//
// BRAINSTORM (pre): the L2 semantic cache exposes a "thumbs-down" admin
// channel (POST /api/admin/cache/semantic-feedback) that adds an
// (entryKey, vkScope) pair to a Redis-backed poison list. Subsequent
// L2 lookups against a poisoned entry are forced to MISS (see
// packages/ai-gateway/internal/cache/semantic/poison.go +
// IsPoisoned wiring in semantic.Reader at lookup.go:508). The white-box
// analogue is already covered by semantic_feedback_test.go; this
// scenario closes the E2E gap — proves the wire path admin-API → Redis
// → ai-gateway lookup actually causes a fresh upstream call.
//
// Endpoint contract (confirmed from
// packages/control-plane/internal/ai/cache/handler/semantic_feedback.go):
//
//	POST /api/admin/cache/semantic-feedback
//	Body: {"entryKey": "<L2 Redis HASH key>", "vkScope": "<vk.id>",
//	       "reason": "<text>", "ttlSeconds": 0}
//	200  → {"status": "poisoned", ...}
//	404  → endpoint not wired = wiring regression on local target (HARD FAIL)
//	503  → Redis client not configured on CP = backend regression (HARD FAIL)
//
// Key construction (per packages/ai-gateway/internal/cache/semantic/
// client.go:237-242 and lookup.go:175):
//
//	entryKey = "<redis_index_name>:<sha256(EmbeddingInput)[:16]>"
//
// where EmbeddingInput is the canonical input-staging string. For a
// single-user-message prompt with EmbedStrategy=system_plus_last_user
// (the default), EmbeddingInput == prompt text verbatim
// (packages/ai-gateway/internal/ingress/proxy/proxy_l2.go:126-153).
// redis_index_name is read from the semantic_cache_config singleton.
//
// Note on L1 vs L2: the exact-match L1 cache is keyed on the canonical
// request body hash (SHA-256 of PrepareBody output). The L2 poison list
// only forces L2 misses; L1 hits are unaffected. To drive the L2 read
// path so the poison can fire, request 3 uses a slightly-perturbed
// prompt (one trailing digit changed) — different L1 key (miss) but
// embedding cosine still ≥ 0.96 against request 1's entry, so L2's
// FT.SEARCH returns the same Redis HASH key, the poison check fires,
// and the gateway falls through to a fresh upstream call (id3 != id2).
//
// Assertions:
//  1. Two warm-up requests return 200 with identical chat IDs (L1+L2
//     cache HIT, baseline established).
//  2. The cache-hit row appears in traffic_event within ~30 s,
//     attributed to this VK via identity->'vk'->>'id'.
//  3. POST cache-feedback returns 200 (or 404/503 → graceful skip).
//  4. Third request with the perturbed prompt returns a DIFFERENT chat
//     ID than request 2 — L2 eviction via poison confirmed.
//  5. nexus_aigw_cache_writes_total delta from before request 3 to
//     after is ≥ 1 (the new write replacing the evicted entry).
//     Metric absent → log warning instead of fail.
func TestS066_CacheNegativeFeedback(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s066-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// Deterministic prompt — per-test nonce isolates this scenario's
	// cache key from any sibling cache scenario.
	prompt := fmt.Sprintf("Reply with exactly: CACHE_TEST_S066. nonce=%d", time.Now().UnixNano())
	buildBody := func(p string) []byte {
		return mustMarshal(t, map[string]any{
			"model": "moonshot-v1-8k",
			"messages": []map[string]string{
				{"role": "user", "content": p},
			},
			"max_tokens":  8,
			"temperature": 0,
		})
	}

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

	// ─── Step 1: warm cache (L1 write + L2 write) ────────────────────────
	// Identical-body replay would L1-hit on canonical body hash and never
	// exercise the L2 read path — leaving gateway_cache_l2_entry_key
	// un-stamped on traffic_event. To drive the L2 read (and the column
	// stamp at proxy_l2.go:277), req2 uses a slightly-perturbed prompt
	// (trailing " .") so the L1 hash differs but the embedding cosine
	// stays well above the 0.96 threshold — same semantic entry, L2 HIT.
	status1, body1, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", buildBody(prompt))
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

	// L2 write-back is async (scheduleL2Write fires in a 5 s-bounded goroutine
	// after the live upstream returns); 4 s gives it a comfortable margin.
	time.Sleep(4 * time.Second)

	// ─── Step 2: confirm cache HIT via L2 (perturbed-prompt path) ────────
	promptPre := prompt + " ."
	status2, body2, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", buildBody(promptPre))
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
	if id1 != id2 {
		t.Fatalf("preconditions failed: req2 did not L2-HIT (id1=%s id2=%s) — without an L2 hit the entry key cannot be stamped and the poison test cannot proceed; check semantic_cache_config.enabled and the 0.96 similarity threshold", id1, id2)
	}

	// ─── Step 3: find the traffic_event row for the cache-hit request ──
	// gateway_cache_status is the canonical column (E72 rename); legacy
	// cache_status is derived from it. We match VK via the identity blob
	// the same way embeddings_test.go does. Poll up to 30 s (5 × 6 s).
	var eventID string
	{
		const tries = 5
		const interval = 6 * time.Second
		for i := 0; i < tries; i++ {
			err := sc.DB.QueryRow(ctx, `
				SELECT id FROM traffic_event
				WHERE source = 'ai-gateway'
				  AND identity->'vk'->>'id' = $1
				  AND gateway_cache_status IN ('hit', 'hit_inflight')
				  AND "timestamp" > NOW() - INTERVAL '120 seconds'
				ORDER BY created_at DESC
				LIMIT 1
			`, vk.ID).Scan(&eventID)
			if err == nil && eventID != "" {
				break
			}
			// pgx returns ErrNoRows on empty; only fatal on a real error.
			if err != nil && !strings.Contains(err.Error(), "no rows") {
				t.Fatalf("traffic_event poll (attempt %d): %v", i+1, err)
			}
			if i < tries-1 {
				time.Sleep(interval)
			}
		}
		if eventID == "" {
			t.Fatalf("no traffic_event row with gateway_cache_status='hit' for VK %s within 30 s — cache-hit row not visible to admin API, cannot poison", vk.ID)
		}
	}

	// ─── Step 4: read the authoritative L2 entry key from traffic_event ─
	// The 2026-05-21 fix added `gateway_cache_l2_entry_key` to traffic_event
	// stamped by AIGw proxy_l2.go on every L2 HIT. Read it directly — the
	// stamped value is the exact Redis HASH key the poison check uses.
	//
	// E86 hardening: NO SHA-256 fallback path. If the stamp is missing
	// after a confirmed cache HIT (id1==id2), that is a backend bug
	// (proxy_l2.go failed to stamp the column on L2 read) and the test
	// fails hard so the regression is caught at this layer rather than
	// papered over with a re-derived key that might match by coincidence.
	var l2EntryKey string
	for i := 0; i < 6; i++ {
		err := sc.DB.QueryRow(ctx,
			`SELECT COALESCE(gateway_cache_l2_entry_key, '')
			   FROM traffic_event
			  WHERE identity->'vk'->>'id' = $1
			    AND gateway_cache_l2_entry_key IS NOT NULL
			    AND gateway_cache_l2_entry_key != ''
			  ORDER BY "timestamp" DESC LIMIT 1`,
			vk.ID).Scan(&l2EntryKey)
		if err == nil && l2EntryKey != "" {
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if l2EntryKey == "" {
		t.Fatalf("S-066: traffic_event.gateway_cache_l2_entry_key is empty/NULL for vk=%s after a confirmed cache HIT (id1=%s id2=%s) — AIGw proxy_l2.go must stamp the column on every L2 read; absence here is a backend regression, not a test-env quirk",
			vk.ID, id1, id2)
	}
	t.Logf("S-066 entryKey from traffic_event stamp: %s", l2EntryKey)

	fbBody := mustMarshal(t, map[string]any{
		"entryKey":   l2EntryKey,
		"vkScope":    vk.ID,
		"reason":     "s066-e2e: negative-feedback eviction test",
		"ttlSeconds": 0, // 0 → server default
	})
	// eventID is still useful for diagnostics in the final t.Logf — keep
	// the column reference so the polling step retains test value.
	_ = eventID
	preEvictMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics pre-eviction: %v", err)
	}
	fbStatus, fbResp, err := helpers.CPDoJSON(ctx, sc.Env, token, http.MethodPost, "/api/admin/cache/semantic-feedback", fbBody)
	if err != nil {
		t.Fatalf("cache-feedback POST: %v", err)
	}
	switch fbStatus {
	case http.StatusOK, http.StatusNoContent:
		// fall through to eviction verification
	case http.StatusNotFound:
		// E86 hardening: 404 = E68 cache-feedback handler not mounted on
		// the local CP — that is a wiring regression, not an env quirk.
		t.Fatalf("S-066: POST /api/admin/cache/semantic-feedback returned 404 — E68 endpoint not wired on the local CP build (body=%q)", truncate(fbResp, 200))
	case http.StatusServiceUnavailable:
		// 503 = CP has no Redis client wired. On the local stack Valkey
		// is always up (docker-compose), so 503 here means the CP didn't
		// resolve the client — backend regression, fail hard.
		t.Fatalf("S-066: POST /api/admin/cache/semantic-feedback returned 503 — CP failed to reach the Redis-backed poison list, but local Valkey is always up; backend regression (body=%q)", truncate(fbResp, 200))
	default:
		t.Fatalf("cache-feedback POST expected 200/204, got %d (%q)", fbStatus, truncate(fbResp, 200))
	}

	// Eviction propagation window.
	time.Sleep(2 * time.Second)

	// ─── Step 5: verify eviction via L2 poison (id3 != id2) ────────────
	// Use a different perturbation than req2's (trailing "  " — two spaces)
	// so the L1 hash differs from req1/req2 (L1 misses cleanly) but the
	// embedding stays far above the 0.96 similarity threshold — L2's
	// FT.SEARCH returns the req1 entry, the poison check on its Redis
	// HASH key fires, and the gateway falls through to a fresh upstream
	// call.
	promptPerturbed := prompt + "  "
	status3, body3, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", buildBody(promptPerturbed))
	if err != nil {
		t.Fatalf("req3 AIGwPostJSON: %v", err)
	}
	if status3 != 200 {
		t.Fatalf("req3 expected 200, got %d (%q)", status3, truncate(body3, 200))
	}
	id3 := parseID(body3)
	if id3 == "" {
		t.Fatalf("req3 missing chat completion id (body=%q)", truncate(body3, 200))
	}
	// ─── Verify the load-bearing contract: POST /semantic-feedback wrote
	// the poison marker into Valkey at the correct key. Format per
	// packages/ai-gateway/internal/cache/semantic/poison.go:67-68 —
	// "nexus:l2:poison:<vkScope>:<entryKey>". This is the contract the
	// feedback admin endpoint owns (write to poison list). The Reader-side
	// IsPoisoned consumption is a separate code path (lookup.go:508)
	// covered by reader_e68_test.go unit tests with a mock PoisonList.
	expectedPoisonKey := "nexus:l2:poison:" + vk.ID + ":" + l2EntryKey
	existsCmd := exec.Command("docker", "exec", "nexus-valkey", "redis-cli", "EXISTS", expectedPoisonKey)
	existsOut, existsErr := existsCmd.CombinedOutput()
	if existsErr != nil {
		t.Fatalf("docker exec redis-cli EXISTS %s: %v (out=%q)", expectedPoisonKey, existsErr, string(existsOut))
	}
	if strings.TrimSpace(string(existsOut)) != "1" {
		t.Errorf("S-066 poison write failed: EXISTS %s returned %q (want \"1\") — POST /semantic-feedback succeeded (status=%d) but the poison marker is not in Valkey",
			expectedPoisonKey, strings.TrimSpace(string(existsOut)), fbStatus)
	}

	// E86 hardening: id3 != id2 is the load-bearing observable proof that
	// the poison fired — the perturbed-prompt request fell through to a
	// fresh upstream call. Previously this was only logged; now hard.
	if id3 == id2 {
		t.Fatalf("S-066 eviction did not fire: id3=%s == id2=%s — poison marker present at %s but the L2 lookup still returned the cached entry; check IsPoisoned wiring at lookup.go:508",
			id3, id2, expectedPoisonKey)
	}

	// E86 hardening: writesDelta ≥ 1 is the corroborating metric signal
	// (a fresh upstream call writes a new cache entry). Previously logged
	// only; now asserted. Metric absence is also a hard fail — the
	// counter is registered at AIGw boot.
	postEvictMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post-eviction: %v", err)
	}
	const writesMetric = "nexus_aigw_cache_writes_total"
	writesDelta := postEvictMetrics.CounterSum(writesMetric, nil) -
		preEvictMetrics.CounterSum(writesMetric, nil)
	if writesDelta < 1 {
		t.Fatalf("S-066: %s delta=%.0f after eviction (want ≥1 — the fresh upstream call must produce a cache write replacing the evicted entry)",
			writesMetric, writesDelta)
	}

	t.Logf("S-066 OK: id1=%s id2=%s id3=%s evicted=true feedback_status=%d writes_delta=%.0f l2EntryKey=%s eventID=%s",
		id1, id2, id3, fbStatus, writesDelta, l2EntryKey, eventID)
}
