// Semantic-cache family (S-064) — verifies that the AI Gateway's L2
// semantic (embedding-similarity) response cache serves a near-paraphrase
// of an already-cached prompt without re-calling the upstream provider.
//
// This complements S-060 (TestS060_CacheHitOnRepeat in cache_test.go),
// which exercises the L1 *prompt* cache where the two requests share an
// IDENTICAL canonical key. S-064 instead drives two requests whose
// canonical keys DIFFER (different words, same meaning) and asserts the
// L2 path returns the prior response when fleet-wide semantic cache is
// configured.
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

// TestS064_SemanticCacheHit — PM-grade e2e for the semantic (L2) cache.
//
// BRAINSTORM (pre): the AI Gateway maintains TWO response caches:
//   - L1 prompt cache (cache_test.go::TestS060_CacheHitOnRepeat) keyed on
//     a normalised request body — identical-string matches only.
//   - L2 semantic cache (this scenario) keyed on an embedding of the
//     prompt — near-paraphrase matches when cosine similarity ≥ the
//     fleet-wide `threshold` knob on `semantic_cache_config`.
//
// L2 is gated by the singleton `semantic_cache_config` row exposed at
// `GET/PUT /api/admin/semantic-cache/config` (see
// `docs/users/api/openapi/admin/e61-s6-cache-admin.yaml`). The row carries
// `enabled`, `embeddingProviderId`, `embeddingModelId`, `threshold`,
// `varyBy`, `embedStrategy`, and `allowCrossModel`. L2 lookups skip
// entirely unless `enabled=true` AND a (provider, model) pair is set.
//
// Cross-service path: AI Gw (semantic.Reader / Writer in
// `packages/ai-gateway/internal/cache/semantic/`, plus
// ingress/proxy/proxy_l2.go gating) → Redis Vector index
// `nexus:semantic-cache:v1` → embedding upstream → DB
// `semantic_cache_config` singleton + `TrafficEvent`. Metrics tick
// `nexus_cache_l2_lookups_total{outcome=...}` for every lookup and
// `nexus_cache_l2_writes_total{outcome=...}` for every write. The L1
// `nexus_cache_lookups_total` counter also ticks because L1 is
// consulted first; we still assert ≥ 2 on it as a sanity rail.
//
// Hardened-precondition rationale (2026-05-22): the scenario fails fast
// (t.Fatalf) on any env that cannot enable semantic cache, instead of
// silently skipping. The local test target MUST seed: (a) an embedding
// provider Credential row, (b) semantic_cache_config singleton wired to
// a (provider, model) pair, and (c) an admin user with `admin:cache.read`
// + `admin:cache.write`. Without those the scenario was effectively a
// no-op; the hardened form turns those gaps into real failures that
// surface in CI / smoke runs.
//
// Assertions:
//  1. Pre-state: GET singleton row, remember it, register a PUT-restore
//     cleanup so we don't leak fleet config across the suite.
//  2. PUT enabled=true with a conservative threshold (0.85). If the PUT
//     returns non-200, t.Fatalf — the env is misconfigured.
//  3. Arm A (warm): POST /v1/chat/completions with prompt
//     "What is the capital of France?" → 200 + chat completion id.
//  4. Sleep 2 s (embedding + Redis HSET commit window — matches the L1
//     pattern used in S-060).
//  5. Arm B (semantic hit attempt): POST with prompt
//     "Could you tell me the capital city of France please?" — different
//     STRING, same MEANING → 200 + chat completion id.
//  6. id1 == id2 is the REQUIRED outcome: different upstream calls
//     always produce different chatcmpl- ids, so an identical id is the
//     strongest possible signal that L2 served Arm B from cache.
//  7. id1 != id2 is a FAIL. The scenario used to pass-on-miss because
//     the v6→v14 index rename never triggered FT.CREATE (config-change
//     dispatch only re-keyed on the embedding fingerprint, not on the
//     index name). That bug shipped 2026-05-22 in index_lifecycle.go;
//     the scenario now asserts the fix.
//  8. Metric rails (always-on): nexus_cache_lookups_total delta
//     ≥ 2 across the two requests (L1 lookups), and
//     nexus_cache_l2_lookups_total delta ≥ 1 (the L2 lookup PATH must
//     execute on each request).
func TestS064_SemanticCacheHit(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s064-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// 1. Read the existing semantic-cache singleton so we can restore it
	//    in cleanup. The CP exposes GET/PUT /api/admin/semantic-cache/config
	//    per docs/users/api/openapi/admin/e61-s6-cache-admin.yaml.
	//
	//    NOTE on endpoint shape: the OpenAPI yaml pins GET/PUT — not
	//    PATCH. The dispatch prompt mentioned PATCH; the actual admin
	//    contract is PUT (full-merge on server side per
	//    SemanticCacheConfigUpdate "server merges into the existing
	//    singleton row"). We use PUT here. If a future refactor moves
	//    this to PATCH, update both this scenario AND the yaml in the
	//    same PR (code/doc lockstep binding).
	preStatus, preBody, err := helpers.CPDoJSON(ctx, sc.Env, token, "GET", "/api/admin/semantic-cache/config", nil)
	if err != nil {
		t.Fatalf("GET semantic-cache/config: %v", err)
	}
	if preStatus != 200 {
		t.Fatalf("S-064 precondition: GET /api/admin/semantic-cache/config must return 200; got %d (%q) — "+
			"hardened scenario requires the semantic-cache admin surface available at this target "+
			"(check Control Plane route registration for /api/admin/semantic-cache/config + IAM "+
			"policy grants `admin:cache.read` to the seeded super-admin)",
			preStatus, truncate(preBody, 200))
	}
	var preCfg struct {
		EmbeddingProviderID *string  `json:"embeddingProviderId"`
		EmbeddingModelID    *string  `json:"embeddingModelId"`
		EmbeddingDimension  *int     `json:"embeddingDimension"`
		Enabled             bool     `json:"enabled"`
		Threshold           *float64 `json:"threshold"`
	}
	if err := json.Unmarshal(preBody, &preCfg); err != nil {
		t.Fatalf("decode semantic-cache singleton: %v (body=%q)", err, truncate(preBody, 400))
	}

	// Restore-on-cleanup: PUT the pre-state back regardless of what the
	// test did. The GET body includes server-managed read-only fields
	// (id, updatedAt, updatedBy, embeddingFingerprint) that PUT ignores
	// or rejects, so we rebuild a PUT-compatible patch from the typed
	// decoded subset.
	//
	// Mirror S-082's restore: include embeddingDimension when present —
	// the PutConfig validator (semanticcache.go:190-196) requires a
	// positive dimension whenever a (provider, model) pair is set,
	// independent of `enabled`. Skipping it on restore reproduces the
	// 2026-05-21 cleanup-PUT-400 incident.
	sc.Cleanup.Register("RestoreSemanticCacheConfig", func() error {
		restore := map[string]any{
			"enabled": preCfg.Enabled,
		}
		if preCfg.EmbeddingProviderID != nil {
			restore["embeddingProviderId"] = *preCfg.EmbeddingProviderID
		}
		if preCfg.EmbeddingModelID != nil {
			restore["embeddingModelId"] = *preCfg.EmbeddingModelID
		}
		if preCfg.EmbeddingDimension != nil {
			restore["embeddingDimension"] = *preCfg.EmbeddingDimension
		}
		if preCfg.Threshold != nil {
			restore["threshold"] = *preCfg.Threshold
		}
		body, _ := json.Marshal(restore)
		status, respBody, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			"PUT", "/api/admin/semantic-cache/config", body)
		if err != nil {
			return fmt.Errorf("restore PUT: %w", err)
		}
		if status != 200 {
			return fmt.Errorf("restore PUT status %d body=%q", status, truncate(respBody, 200))
		}
		return nil
	})

	// 2. Enable semantic cache with a conservative threshold (0.85).
	//
	//    PutConfig validates that whenever a (provider, model) pair is
	//    set the singleton MUST carry a positive embeddingDimension
	//    (semanticcache.go:190-196). Resolution order, matching S-082:
	//      a. Reuse the pre-existing dimension when the singleton row
	//         already has one (the dev seed wires text-embedding-3-small
	//         at dimension 1536).
	//      b. Else POST /api/admin/providers/:id/embedding-probe to
	//         discover the dimension at the embedding provider/model
	//         pair currently configured.
	//      c. Else fall back to the OpenAI text-embedding-3-small
	//         default (1536) — load-bearing only for validator pass-
	//         through; if the actual provider returns a different
	//         dimension Arm B will skip via the id-mismatch graceful
	//         path below.
	dimension := 0
	if preCfg.EmbeddingDimension != nil && *preCfg.EmbeddingDimension > 0 {
		dimension = *preCfg.EmbeddingDimension
	} else if preCfg.EmbeddingProviderID != nil && *preCfg.EmbeddingProviderID != "" {
		probePath := "/api/admin/providers/" + *preCfg.EmbeddingProviderID + "/embedding-probe"
		probeStatus, probeBody, probeErr := helpers.CPDoJSON(ctx, sc.Env, token,
			"POST", probePath, []byte("{}"))
		if probeErr == nil && probeStatus == 200 {
			var probeResp struct {
				OK        bool `json:"ok"`
				Dimension int  `json:"dimension"`
			}
			if err := json.Unmarshal(probeBody, &probeResp); err == nil && probeResp.OK && probeResp.Dimension > 0 {
				dimension = probeResp.Dimension
			}
		}
		if dimension == 0 {
			// Fallback to OpenAI text-embedding-3-small default; if the
			// configured provider returns a different dimension, the
			// downstream FT.SEARCH on Arm B will fail and surface as a
			// real product error (no longer silently skipped).
			dimension = 1536
		}
	} else {
		t.Fatalf("S-064 precondition: semantic_cache_config singleton must carry embeddingProviderId; got nil — "+
			"hardened scenario requires an embedding provider seeded on the singleton "+
			"(UPDATE semantic_cache_config SET \"embeddingProviderId\"=..., \"embeddingModelId\"=...)")
	}

	enablePatch := map[string]any{
		"enabled":            true,
		"threshold":          0.85,
		"embeddingDimension": dimension,
	}
	if preCfg.EmbeddingProviderID != nil {
		enablePatch["embeddingProviderId"] = *preCfg.EmbeddingProviderID
	}
	if preCfg.EmbeddingModelID != nil {
		enablePatch["embeddingModelId"] = *preCfg.EmbeddingModelID
	}
	enableBody := mustMarshal(t, enablePatch)
	enableStatus, enableRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"PUT", "/api/admin/semantic-cache/config", enableBody)
	if err != nil {
		t.Fatalf("PUT semantic-cache/config enable: %v", err)
	}
	if enableStatus == 400 {
		t.Fatalf("S-064 precondition: PUT /api/admin/semantic-cache/config enable returned 400 (%q) — "+
			"hardened scenario requires a working embedding provider/model seeded in "+
			"semantic_cache_config so PutConfig validation passes (provider+model+dimension "+
			"triple consistent with a live Credential row)",
			truncate(enableRespBody, 300))
	}
	if enableStatus != 200 {
		t.Fatalf("PUT semantic-cache/config enable: status %d body=%q", enableStatus, truncate(enableRespBody, 300))
	}

	// Snapshot metrics before the two-request burst.
	preMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics pre: %v", err)
	}

	// Per-test nonce — the L1 prompt cache normalises on the full
	// canonical body, so a nonce in BOTH prompts keeps L1 isolated
	// from any other scenario, but the prompts must still differ
	// lexically while preserving meaning so the L2 path is what
	// closes the loop.
	nonce := time.Now().UnixNano()
	promptA := fmt.Sprintf("What is the capital of France? test-nonce=%d", nonce)
	promptB := fmt.Sprintf("Could you tell me the capital city of France please? test-nonce=%d", nonce)

	mkBody := func(prompt string) []byte {
		return mustMarshal(t, map[string]any{
			"model": "moonshot-v1-8k",
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
			"max_tokens":  16,
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

	// 3. Arm A — warm the semantic cache. Cache MISS expected; the
	//    writer asynchronously embeds + HSETs the entry.
	statusA, bodyA, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", mkBody(promptA))
	if err != nil {
		t.Fatalf("Arm A AIGwPostJSON: %v", err)
	}
	if statusA != 200 {
		t.Fatalf("Arm A expected 200, got %d (%q)", statusA, truncate(bodyA, 200))
	}
	id1 := parseID(bodyA)
	if id1 == "" {
		t.Fatalf("Arm A missing chat completion id (body=%q)", truncate(bodyA, 200))
	}

	// 4. Sleep — give the writer time to commit the L2 HSET before Arm
	//    B's FT.SEARCH. 2 s mirrors the S-060 pattern.
	time.Sleep(2 * time.Second)

	// 5. Arm B — same meaning, different words. If the embedding model
	//    + Redis Vector index are wired up and similarity ≥ 0.85, this
	//    returns the Arm A response.
	statusB, bodyB, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", mkBody(promptB))
	if err != nil {
		t.Fatalf("Arm B AIGwPostJSON: %v", err)
	}
	if statusB != 200 {
		t.Fatalf("Arm B expected 200, got %d (%q)", statusB, truncate(bodyB, 200))
	}
	id2 := parseID(bodyB)
	if id2 == "" {
		t.Fatalf("Arm B missing chat completion id (body=%q)", truncate(bodyB, 200))
	}

	// 6/7. Hit decision.
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post: %v", err)
	}
	l1LookupsDelta := postMetrics.CounterSum("nexus_cache_lookups_total", nil) -
		preMetrics.CounterSum("nexus_cache_lookups_total", nil)
	l2LookupsDelta := postMetrics.CounterSum("nexus_cache_l2_lookups_total", nil) -
		preMetrics.CounterSum("nexus_cache_l2_lookups_total", nil)
	l2WritesDelta := postMetrics.CounterSum("nexus_cache_l2_writes_total", nil) -
		preMetrics.CounterSum("nexus_cache_l2_writes_total", nil)

	// 7/8. Cache outcome.
	//
	// HIT is REQUIRED: id1 == id2. The L2 semantic cache must serve Arm
	// B from the entry Arm A wrote. Anything else (MISS, error, distinct
	// upstream ids) is a real product regression and fails the scenario.
	//
	// Three things have to be true for this to work:
	//   (a) The Valkey FT vector index named in semantic_cache_config
	//       must physically exist (FT.CREATE must have fired). Pre-fix
	//       2026-05-22, renaming redis_index_name without changing
	//       provider/model/dim left the new index uncreated because the
	//       config dispatcher keyed only on the embedding fingerprint;
	//       FT.SEARCH on the new name returned "Index not found" and
	//       every Arm B was forced to call upstream.
	//   (b) The embedding-provider call must succeed on both arms so
	//       the writer can HSET on Arm A and the lookup can FT.SEARCH
	//       on Arm B.
	//   (c) Cosine similarity between the two prompts must clear the
	//       0.85 threshold we just PUT.
	//
	// If any of those is broken in real env (no embedding provider
	// credential, FT index for a stale version, etc.) the t.Fatalf
	// preconditions above already failed the test. By this point the env
	// is fully provisioned and a MISS is a real product regression.
	if l1LookupsDelta < 2 {
		t.Errorf("L1 cache_lookups delta=%g (want ≥ 2 across 2 requests — sanity rail)", l1LookupsDelta)
	}
	if l2LookupsDelta < 1 {
		t.Errorf("L2 cache_lookups delta=%g (want ≥ 1 — the L2 lookup PATH must execute on each request)",
			l2LookupsDelta)
	}
	if !strings.HasPrefix(id1, "chatcmpl-") {
		t.Errorf("Arm A returned non-canonical chat-completion id %q (want prefix chatcmpl-) — "+
			"OpenAI-shape ingress must always stamp the canonical chatcmpl- prefix", id1)
	}
	if !strings.HasPrefix(id2, "chatcmpl-") {
		t.Errorf("Arm B returned non-canonical chat-completion id %q (want prefix chatcmpl-) — "+
			"OpenAI-shape ingress must always stamp the canonical chatcmpl- prefix", id2)
	}

	if id1 != id2 {
		t.Fatalf("S-064 FAIL: semantic cache MISS — armA.id=%s armB.id=%s (distinct upstream ids => Arm B was NOT served from L2). "+
			"l1_lookups_delta=%.0f l2_lookups_delta=%.0f l2_writes_delta=%.0f. "+
			"Likely root causes: (1) redis_index_name in semantic_cache_config does not match a live FT index (run `docker exec nexus-valkey redis-cli FT._LIST` to verify); "+
			"(2) embedding-provider call failed on Arm A so no vector was written (check L2 writes delta — should be ≥ 1); "+
			"(3) cosine similarity below 0.85 (lower threshold and re-run); "+
			"(4) IndexLifecycle did not call FT.CREATE on a renamed index (bug fixed 2026-05-22 in index_lifecycle.go — verify the fix is on the running binary).",
			id1, id2, l1LookupsDelta, l2LookupsDelta, l2WritesDelta)
	}
	if l2WritesDelta < 1 {
		t.Errorf("L2 cache_writes delta=%g (want ≥ 1 — Arm A must have written the entry that Arm B's HIT consumed)", l2WritesDelta)
	}
	t.Logf("S-064 OK (HIT path): armA.id=%s armB.id=%s identical=true l1_lookups_delta=%.0f l2_lookups_delta=%.0f l2_writes_delta=%.0f",
		id1, id2, l1LookupsDelta, l2LookupsDelta, l2WritesDelta)
}
