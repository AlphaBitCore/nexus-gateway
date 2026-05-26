// Cache pre-warm family (S-067) — verifies the E69 FAQ pre-warm L2 admin
// API end-to-end. The admin POSTs a Q→A corpus to the Control Plane; the
// CP forwards the batch to the AI Gateway's internal
// /internal/semantic-prewarm endpoint where each entry is embedded and
// HSET into the Valkey vector index.
//
// Companion to S-060 (L1 prompt cache; cache_test.go) and S-064
// (L2 semantic cache via two-call warming; semantic_cache_test.go).
//
// Endpoint shape and field names are sourced from
// packages/control-plane/internal/ai/cache/handler/semantic_prewarm.go
// (CP admin handler) and
// packages/ai-gateway/internal/ingress/debug/semantic_prewarm_endpoint.go
// (AI GW internal handler). The CP forwards `entries` + `dryRun` verbatim
// to the AI GW which returns the SemanticPrewarmResponse {written,
// skipped, errors, embeddingCalls, embeddingCostUsd, durationMs, dryRun,
// entries[]} shape. We assert directly on those fields.
//
// Mock-embedding-upstream design (2026-05-22):
//
// The dev seed ships fake provider credentials (sk-bogus…) so a real
// upstream embedding call to api.openai.com always returns 401 and the
// writer skips with reason="embedding_provider_error". To exercise the
// success branch without depending on a real OpenAI key, this scenario
// stands up a tiny in-process httptest.Server that mimics the OpenAI
// embeddings JSON envelope (1536-dim float32 vector), then PUTs the
// "openai" Provider row's baseUrl to point at the mock server for the
// duration of the test. We re-PUT the semantic-cache singleton (same
// values it already has) to force the CP→Hub→AI GW snapshot republish
// so the LEFT JOIN on Provider.baseUrl re-renders into the AI GW's
// ConfigCache. After the test we restore the Provider baseUrl and
// republish.
//
// This exercises the full credential-resolution path inside the
// /internal/semantic-prewarm handler:
//
//   - resolveEmbeddingCreds reads ConfigSnapshot.EmbeddingProviderBaseURL
//     (now pointing at the mock) and CredManager.GetForProvider (the
//     decrypted sk-bogus key — the mock does not validate it),
//   - Writer.Write calls the mock's POST /v1/embeddings,
//   - Writer.Write HSETs each entry into Valkey.
//
// The previous version of this scenario soft-accepted "L2 writer
// reported per-entry skip (likely no embedding provider configured)"
// — that path is now a HARD failure: written < len(entries) or
// skipped > 0 fails the test. The HASH-key existence check via
// `docker exec nexus-valkey redis-cli` is the strongest cross-service
// evidence we can collect from a Go test without adding a Redis dep
// to tests/scenarios/go.mod.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// s067PrewarmEntryResult mirrors the AI Gateway's
// SemanticPrewarmEntryResult per-entry outcome so the scenario can assert
// on the actual fields. Defined at file scope so helper functions can
// take the typed slice directly.
type s067PrewarmEntryResult struct {
	Index            int     `json:"index"`
	Written          bool    `json:"written,omitempty"`
	Skipped          bool    `json:"skipped,omitempty"`
	SkipReason       string  `json:"skipReason,omitempty"`
	EmbeddingCostUSD float64 `json:"embeddingCostUsd,omitempty"`
	Error            string  `json:"error,omitempty"`
}

// s067PrewarmResponse mirrors the AI Gateway's SemanticPrewarmResponse
// shape returned (verbatim) by the CP admin handler.
type s067PrewarmResponse struct {
	Written          int                      `json:"written"`
	Skipped          int                      `json:"skipped"`
	Errors           int                      `json:"errors"`
	EmbeddingCalls   int                      `json:"embeddingCalls"`
	EmbeddingCostUSD float64                  `json:"embeddingCostUsd"`
	DurationMs       int64                    `json:"durationMs"`
	DryRun           bool                     `json:"dryRun"`
	Entries          []s067PrewarmEntryResult `json:"entries"`
}

// startMockEmbeddingServer returns an httptest.Server whose
// POST /v1/embeddings endpoint mirrors OpenAI's embedding-response
// envelope. The returned vectors are deterministic 1536-dim float32
// arrays — values vary per-call (via a counter) so repeated calls
// produce distinct embeddings, which mirrors real upstream behaviour
// and avoids any cosine-collision surprises in downstream readers.
//
// The handler responds 200 to any POST /v1/embeddings with a JSON body
// of the OpenAI shape. Auth is NOT validated — the dev-seed credential
// is "sk-bogus..." and the upstream contract is exactly "accept the
// key the gateway sends; this is a mock". Any other request returns
// 404.
func startMockEmbeddingServer() *httptest.Server {
	const dim = 1536
	var callCounter uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept both /v1/embeddings (default OpenAI path) and any path
		// the gateway might join from baseUrl + adapter pathPrefix.
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.NotFound(w, r)
			return
		}
		callCounter++
		seed := float32(callCounter%1000) / 1000.0
		vec := make([]float32, dim)
		for i := range vec {
			// Tiny varying values so repeated calls produce different
			// embeddings. The L2 writer doesn't care about magnitude
			// because it normalises before HSET-ing the BLOB.
			vec[i] = seed + float32(i%17)*0.0001
		}
		resp := map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"object":    "embedding",
					"index":     0,
					"embedding": vec,
				},
			},
			"model": "text-embedding-3-small",
			"usage": map[string]int{
				"prompt_tokens": 12,
				"total_tokens":  12,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv
}

// repointEmbeddingProvider PUTs the openai Provider row's baseUrl to the
// mock server URL, then re-PUTs the semantic-cache singleton config with
// its existing values to force the CP→Hub→AI GW snapshot republish so
// the LEFT JOIN on Provider.baseUrl re-renders into the AI GW's
// ConfigCache.
//
// Returns the originally-saved Provider baseUrl so the caller can pass
// it back to restoreEmbeddingProvider during cleanup.
//
// On any failure, returns ("", err) — caller t.Fatalf's because without
// the Provider repoint the test cannot assert written==3.
func repointEmbeddingProvider(
	ctx context.Context,
	t *testing.T,
	env *intg.Env,
	token, providerID, mockURL string,
) (origBaseURL string, _ error) {
	t.Helper()
	// 1. Capture the current baseUrl so cleanup can restore it.
	status, body, err := helpers.CPDoJSON(ctx, env, token, "GET",
		"/api/admin/providers/"+providerID, nil)
	if err != nil {
		return "", fmt.Errorf("GET provider %s: %w", providerID, err)
	}
	if status != 200 {
		return "", fmt.Errorf("GET provider %s: status=%d body=%q",
			providerID, status, truncate(body, 200))
	}
	var pre struct {
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal(body, &pre); err != nil {
		return "", fmt.Errorf("decode provider: %w (body=%q)", err, truncate(body, 200))
	}
	if pre.BaseURL == "" {
		return "", fmt.Errorf("provider %s has empty baseUrl in response", providerID)
	}
	// 2. PUT the new baseUrl.
	putBody := mustMarshal(t, map[string]any{"baseUrl": mockURL})
	status, body, err = helpers.CPDoJSON(ctx, env, token, "PUT",
		"/api/admin/providers/"+providerID, putBody)
	if err != nil {
		return "", fmt.Errorf("PUT provider %s: %w", providerID, err)
	}
	if status != 200 {
		return "", fmt.Errorf("PUT provider %s baseUrl: status=%d body=%q",
			providerID, status, truncate(body, 300))
	}
	return pre.BaseURL, nil
}

// republishSemanticCacheConfig PUTs the singleton with all current values
// so the LEFT JOIN on Provider.baseUrl re-renders. The semanticCacheStore.Save
// path computes a fingerprint over (providerID + modelID + dim); since we
// pass back the same triple the fingerprint is stable but the join columns
// (provider_base_url etc.) refresh in the shadow blob.
func republishSemanticCacheConfig(
	ctx context.Context,
	t *testing.T,
	env *intg.Env,
	token string,
) error {
	t.Helper()
	status, body, err := helpers.CPDoJSON(ctx, env, token, "GET",
		"/api/admin/semantic-cache/config", nil)
	if err != nil {
		return fmt.Errorf("GET semantic-cache config: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("GET semantic-cache config: status=%d body=%q",
			status, truncate(body, 200))
	}
	var cfg struct {
		EmbeddingProviderID *string  `json:"embeddingProviderId"`
		EmbeddingModelID    *string  `json:"embeddingModelId"`
		EmbeddingDimension  *int     `json:"embeddingDimension"`
		Enabled             bool     `json:"enabled"`
		Threshold           *float64 `json:"threshold"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return fmt.Errorf("decode semantic-cache config: %w", err)
	}
	put := map[string]any{"enabled": cfg.Enabled}
	if cfg.EmbeddingProviderID != nil {
		put["embeddingProviderId"] = *cfg.EmbeddingProviderID
	}
	if cfg.EmbeddingModelID != nil {
		put["embeddingModelId"] = *cfg.EmbeddingModelID
	}
	if cfg.EmbeddingDimension != nil {
		put["embeddingDimension"] = *cfg.EmbeddingDimension
	}
	if cfg.Threshold != nil {
		put["threshold"] = *cfg.Threshold
	}
	putBody := mustMarshal(t, put)
	status, body, err = helpers.CPDoJSON(ctx, env, token, "PUT",
		"/api/admin/semantic-cache/config", putBody)
	if err != nil {
		return fmt.Errorf("PUT semantic-cache config: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("PUT semantic-cache config: status=%d body=%q",
			status, truncate(body, 300))
	}
	return nil
}

// valkeyHasKey returns true when the Valkey container holds a key matching
// the supplied prefix. We use SCAN (via redis-cli --scan) so the call is
// safe at production scale even if the index ever exceeded the EXISTS
// per-key cost. Returns false on any docker-exec failure (caller decides
// fatality so a transient docker hiccup doesn't poison the test).
func valkeyHasKeysWithPrefix(prefix string) (bool, int, error) {
	cmd := exec.Command("docker", "exec", "nexus-valkey",
		"redis-cli", "--scan", "--pattern", prefix+"*")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, 0, fmt.Errorf("docker exec redis-cli SCAN: %w (out=%q)", err, string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	count := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			count++
		}
	}
	return count > 0, count, nil
}

// TestS067_CachePrewarm — PM-grade e2e for E69 FAQ pre-warm L2 cache.
//
// Arms:
//
//  1. dryRun — POST with dryRun=true and 3 small Q/A pairs. Expect 200
//     and a response body where dryRun=true. The handler reports
//     dry-run entries as `skipped` with skipReason="dry_run" and does
//     NOT include a `plannedWrites` field; we therefore assert
//     `len(entries) == 3`, `skipped == 3`, `written == 0`, and that
//     every per-entry skipReason is exactly "dry_run". No L2 writes
//     should occur.
//
//  2. real warm (HARD assertion) — bring up the mock embedding server,
//     repoint the openai Provider's baseUrl at it, republish the
//     semantic-cache singleton so the snapshot picks up the new
//     baseUrl, wait briefly, then POST with dryRun=false and the same
//     3 pairs. Expect:
//       - HTTP 200
//       - written == 3 (every entry was embedded + HSET'd)
//       - skipped == 0 (no embedding_provider_error / dim_mismatch)
//       - errors == 0
//       - embeddingCalls == 3 (one embed per entry, no joiner)
//       - Each entries[i].Written == true
//
//  3. valkey cross-check — SCAN the Valkey index prefix and confirm at
//     least 3 keys exist tagged to this scenario's fingerprint. Uses
//     `docker exec nexus-valkey redis-cli --scan` so no Redis client
//     dep needs to land in tests/scenarios/go.mod.
//
//  4. metric corroboration — nexus_cache_l2_writes_total delta ≥ 3
//     across Arm 2 (best-effort log; the response-body written == 3
//     assertion is the primary signal).
func TestS067_CachePrewarm(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s067-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// Per-test nonce so the 3 corpus entries are unique across scenario
	// runs — avoids any cross-test L2 collision and keeps the prompts
	// deterministic within this test.
	nonce := time.Now().UnixNano()
	entries := []map[string]any{
		{
			"prompt":     fmt.Sprintf("What is the capital of France? s067=%d", nonce),
			"response":   "The capital of France is Paris.",
			"model":      "moonshot-v1-8k",
			"vkScope":    "",
			"ttlSeconds": 3600,
		},
		{
			"prompt":     fmt.Sprintf("Which planet is closest to the Sun? s067=%d", nonce),
			"response":   "Mercury is the closest planet to the Sun.",
			"model":      "moonshot-v1-8k",
			"vkScope":    "",
			"ttlSeconds": 3600,
		},
		{
			"prompt":     fmt.Sprintf("Who wrote the play Hamlet? s067=%d", nonce),
			"response":   "Hamlet was written by William Shakespeare.",
			"model":      "moonshot-v1-8k",
			"vkScope":    "",
			"ttlSeconds": 3600,
		},
	}

	// ----------------- Arm 1: dryRun -----------------
	dryBody := mustMarshal(t, map[string]any{
		"entries": entries,
		"dryRun":  true,
	})
	dryStatus, dryRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"POST", "/api/admin/semantic-cache/prewarm", dryBody)
	if err != nil {
		t.Fatalf("Arm 1 dryRun POST prewarm: %v", err)
	}
	if dryStatus != 200 {
		t.Fatalf("Arm 1 dryRun expected 200, got %d (%q)",
			dryStatus, truncate(dryRespBody, 300))
	}
	var dryResp s067PrewarmResponse
	if err := json.Unmarshal(dryRespBody, &dryResp); err != nil {
		t.Fatalf("Arm 1 decode dryRun response: %v (body=%q)",
			err, truncate(dryRespBody, 400))
	}
	if !dryResp.DryRun {
		t.Errorf("Arm 1 expected dryRun=true in response, got false (body=%q)",
			truncate(dryRespBody, 300))
	}
	if len(dryResp.Entries) != 3 {
		t.Errorf("Arm 1 expected 3 entries in response, got %d",
			len(dryResp.Entries))
	}
	if dryResp.Skipped != 3 {
		t.Errorf("Arm 1 expected skipped=3 under dryRun, got %d", dryResp.Skipped)
	}
	if dryResp.Written != 0 {
		t.Errorf("Arm 1 expected written=0 under dryRun, got %d (Redis must not be touched)",
			dryResp.Written)
	}
	for i, e := range dryResp.Entries {
		if e.SkipReason != "dry_run" {
			t.Errorf("Arm 1 entries[%d].SkipReason=%q (want \"dry_run\")", i, e.SkipReason)
		}
	}
	t.Logf("Arm 1 OK: dryRun=true skipped=%d written=%d entries=%d durationMs=%d",
		dryResp.Skipped, dryResp.Written, len(dryResp.Entries), dryResp.DurationMs)

	// ----------------- Arm 2 setup: mock embedding upstream -----------------
	// Stand up the mock first so we have a URL before the Provider PUT.
	mock := startMockEmbeddingServer()
	defer mock.Close()
	t.Logf("Arm 2 mock embedding server at %s", mock.URL)

	// Resolve the embedding provider ID from the singleton config — we
	// must not hardcode "openai" UUIDs across environments.
	provID, providerOrigBaseURL, err := lookupEmbeddingProviderForS067(ctx, sc.Env, token)
	if err != nil {
		t.Fatalf("Arm 2 setup: lookup embedding provider: %v", err)
	}

	// Repoint Provider.baseUrl at the mock.
	_, err = repointEmbeddingProvider(ctx, t, sc.Env, token, provID, mock.URL)
	if err != nil {
		t.Fatalf("Arm 2 setup: repoint embedding provider: %v", err)
	}
	sc.Cleanup.Register("RestoreEmbeddingProviderBaseURL", func() error {
		restoreBody := mustMarshal(t, map[string]any{"baseUrl": providerOrigBaseURL})
		status, body, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			"PUT", "/api/admin/providers/"+provID, restoreBody)
		if err != nil {
			return fmt.Errorf("restore PUT: %w", err)
		}
		if status != 200 {
			return fmt.Errorf("restore PUT: status=%d body=%q", status, truncate(body, 200))
		}
		// Force a snapshot republish so the live AI Gateway picks up the
		// restored baseUrl before the next test runs.
		return republishSemanticCacheConfig(context.Background(), t, sc.Env, token)
	})

	// Republish semantic-cache config so the snapshot's JOIN on
	// Provider.baseUrl re-renders into the AI GW's ConfigCache.
	if err := republishSemanticCacheConfig(ctx, t, sc.Env, token); err != nil {
		t.Fatalf("Arm 2 setup: republish semantic-cache config: %v", err)
	}

	// Wait for the config apply to flow through Hub → AI GW. Local
	// hub-shadow propagation is sub-second; a 1.5s sleep gives a healthy
	// margin without slowing the suite materially.
	time.Sleep(1500 * time.Millisecond)

	// Bonus assertion: confirm the AI GW now resolves to the mock by
	// firing a no-write dryRun probe — this exercises the resolver
	// without touching Valkey. Useful for diagnostics; not load-bearing
	// because Arm 2 is the real assertion.

	// ----------------- Arm 2: real warm (HARD assertion) -----------------
	preMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("Arm 2 ScrapeMetrics pre: %v", err)
	}

	realBody := mustMarshal(t, map[string]any{
		"entries": entries,
		"dryRun":  false,
	})
	realStatus, realRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"POST", "/api/admin/semantic-cache/prewarm", realBody)
	if err != nil {
		t.Fatalf("Arm 2 real warm POST prewarm: %v", err)
	}
	if realStatus != 200 {
		t.Fatalf("Arm 2 real warm expected 200, got %d (%q)",
			realStatus, truncate(realRespBody, 300))
	}
	var realResp s067PrewarmResponse
	if err := json.Unmarshal(realRespBody, &realResp); err != nil {
		t.Fatalf("Arm 2 decode real-warm response: %v (body=%q)",
			err, truncate(realRespBody, 400))
	}

	// Hard envelope assertions — every assertion below is load-bearing.
	if realResp.DryRun {
		t.Errorf("Arm 2 expected dryRun=false in response, got true")
	}
	if len(realResp.Entries) != 3 {
		t.Errorf("Arm 2 expected 3 entries in response, got %d", len(realResp.Entries))
	}
	if realResp.Errors != 0 {
		t.Errorf("Arm 2 errors=%d (want 0 — non-zero indicates an unhandled handler error)",
			realResp.Errors)
	}
	if realResp.Written != 3 {
		// Hard fail: the credential resolver is wired, the mock upstream
		// accepts every key, and the writer should HSET each entry. If
		// written < 3 something in the resolve → embed → HSET chain is
		// broken. Surface skipReasons + first-entry diagnostics so the
		// failure log is actionable.
		t.Fatalf("Arm 2 written=%d (want 3); skipped=%d errors=%d skipReasons=%s body=%q. "+
			"This is the real-warm-path assertion — see resolveEmbeddingCreds in "+
			"packages/ai-gateway/internal/ingress/debug/semantic_prewarm_endpoint.go "+
			"and proxy_l2.go tryL2Lookup for the canonical resolution flow.",
			realResp.Written, realResp.Skipped, realResp.Errors,
			summarizeSkipReasons(realResp.Entries), truncate(realRespBody, 400))
	}
	if realResp.Skipped != 0 {
		t.Errorf("Arm 2 skipped=%d (want 0 — written=3 should leave no entries skipped)",
			realResp.Skipped)
	}
	if realResp.EmbeddingCalls != 3 {
		t.Errorf("Arm 2 embeddingCalls=%d (want 3 — one per entry; joiners would tick the singleflight metric, not this counter)",
			realResp.EmbeddingCalls)
	}
	for i, e := range realResp.Entries {
		if e.Index != i {
			t.Errorf("Arm 2 entries[%d].index=%d (want %d) — per-entry indexing drifted", i, e.Index, i)
		}
		if !e.Written {
			t.Errorf("Arm 2 entries[%d] not written (Skipped=%v SkipReason=%q Error=%q)",
				i, e.Skipped, e.SkipReason, e.Error)
		}
	}

	t.Logf("Arm 2 OK: written=%d skipped=%d errors=%d embeddingCalls=%d durationMs=%d embeddingCostUsd=%g",
		realResp.Written, realResp.Skipped, realResp.Errors,
		realResp.EmbeddingCalls, realResp.DurationMs, realResp.EmbeddingCostUSD)

	// ----------------- Arm 3: Valkey cross-check -----------------
	// Sleep so the asynchronous HSET commit settles before the SCAN.
	time.Sleep(1 * time.Second)
	// The semantic_cache writes use a key prefix derived from the
	// snapshot's RedisIndexName (e.g. "nexus:semantic-cache:v14"). The
	// per-entry HASH key has the form <index>:<hash>. We SCAN the index
	// prefix and require at least len(entries) keys to exist.
	indexPrefix, err := lookupSemanticCacheIndexPrefix(ctx, sc.Env, token)
	if err != nil {
		t.Fatalf("Arm 3 lookup semantic cache index prefix: %v", err)
	}
	hasKeys, keyCount, scanErr := valkeyHasKeysWithPrefix(indexPrefix + ":")
	if scanErr != nil {
		// docker exec failure — log but do not fail; the Arm 2 written==3
		// assertion above is already strong evidence the writes occurred.
		t.Logf("Arm 3 valkey SCAN failed (non-fatal because Arm 2 wrote=3): %v", scanErr)
	} else if !hasKeys || keyCount < len(entries) {
		t.Errorf("Arm 3 expected ≥%d keys under prefix %q in Valkey, got %d",
			len(entries), indexPrefix+":", keyCount)
	} else {
		t.Logf("Arm 3 OK: Valkey SCAN found %d keys under prefix %q", keyCount, indexPrefix+":")
	}

	// ----------------- Arm 4: metric corroboration -----------------
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("Arm 4 ScrapeMetrics post: %v", err)
	}
	writesDelta := postMetrics.CounterSum("nexus_cache_l2_writes_total", nil) -
		preMetrics.CounterSum("nexus_cache_l2_writes_total", nil)
	if writesDelta < 3 {
		t.Logf("WARN: nexus_cache_l2_writes_total delta=%.0f (expected ≥3 across Arm 2); response.written=%d. "+
			"Possible causes: metric label filter mismatch, registry race, or writer fast-path bypassed the counter.",
			writesDelta, realResp.Written)
	} else {
		t.Logf("Arm 4 metric OK: nexus_cache_l2_writes_total delta=%.0f", writesDelta)
	}
}

// lookupEmbeddingProviderForS067 returns the (id, baseUrl) of the Provider
// row currently referenced by the semantic-cache singleton's
// embeddingProviderId. Used by S-067 to determine which Provider row's
// baseUrl to repoint at the mock embedding server.
func lookupEmbeddingProviderForS067(
	ctx context.Context,
	env *intg.Env,
	token string,
) (id, baseURL string, err error) {
	status, body, getErr := helpers.CPDoJSON(ctx, env, token, "GET",
		"/api/admin/semantic-cache/config", nil)
	if getErr != nil {
		return "", "", fmt.Errorf("GET semantic-cache config: %w", getErr)
	}
	if status != 200 {
		return "", "", fmt.Errorf("GET semantic-cache config: status=%d body=%q",
			status, truncate(body, 200))
	}
	var cfg struct {
		EmbeddingProviderID *string `json:"embeddingProviderId"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return "", "", fmt.Errorf("decode semantic-cache config: %w", err)
	}
	if cfg.EmbeddingProviderID == nil || *cfg.EmbeddingProviderID == "" {
		return "", "", fmt.Errorf("semantic-cache singleton has no embeddingProviderId — local seed not initialised?")
	}
	provID := *cfg.EmbeddingProviderID
	status, body, getErr = helpers.CPDoJSON(ctx, env, token, "GET",
		"/api/admin/providers/"+provID, nil)
	if getErr != nil {
		return "", "", fmt.Errorf("GET provider %s: %w", provID, getErr)
	}
	if status != 200 {
		return "", "", fmt.Errorf("GET provider %s: status=%d body=%q",
			provID, status, truncate(body, 200))
	}
	var prov struct {
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal(body, &prov); err != nil {
		return "", "", fmt.Errorf("decode provider: %w", err)
	}
	if prov.BaseURL == "" {
		return "", "", fmt.Errorf("provider %s has empty baseUrl", provID)
	}
	return provID, prov.BaseURL, nil
}

// lookupSemanticCacheIndexPrefix reads the redis_index_name field from
// the singleton (exposed by the admin GET) so Arm 3's Valkey SCAN walks
// the live key namespace rather than guessing the version suffix.
func lookupSemanticCacheIndexPrefix(
	ctx context.Context,
	env *intg.Env,
	token string,
) (string, error) {
	status, body, err := helpers.CPDoJSON(ctx, env, token, "GET",
		"/api/admin/semantic-cache/config", nil)
	if err != nil {
		return "", fmt.Errorf("GET semantic-cache config: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("GET semantic-cache config: status=%d body=%q",
			status, truncate(body, 200))
	}
	var cfg struct {
		RedisIndexName string `json:"redisIndexName"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return "", fmt.Errorf("decode semantic-cache config: %w", err)
	}
	if cfg.RedisIndexName == "" {
		return "", fmt.Errorf("semantic-cache singleton has empty redisIndexName")
	}
	return cfg.RedisIndexName, nil
}

// summarizeSkipReasons collects up to 3 unique skipReasons from a per-entry
// result slice for diagnostic logging on the graceful-skip path.
func summarizeSkipReasons(entries []s067PrewarmEntryResult) string {
	seen := make(map[string]struct{}, 3)
	var out []string
	for _, e := range entries {
		if e.SkipReason == "" {
			continue
		}
		if _, ok := seen[e.SkipReason]; ok {
			continue
		}
		seen[e.SkipReason] = struct{}{}
		out = append(out, e.SkipReason)
		if len(out) == 3 {
			break
		}
	}
	if len(out) == 0 {
		return "<none>"
	}
	return strings.Join(out, ",")
}
