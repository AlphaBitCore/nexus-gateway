// Embedding-provider-config family (S-082) — verifies that the
// fleet-wide embedding (provider, model) pair on the semantic-cache
// singleton config can be round-tripped through the admin API. This is
// the configuration surface that gates which embedding upstream the
// AI Gateway's L2 semantic cache (and any other L1 semantic consumer)
// will call. Closes the admin-write coverage gap for E61-S5.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS082_EmbeddingProviderConfig — PM-grade e2e for the fleet-wide
// embedding (provider, model) pair on `semantic_cache_config`.
//
// BRAINSTORM (pre): the admin surface exposes a singleton row at
// `GET/PUT /api/admin/semantic-cache/config` (see semanticcache.go:128
// — `EmbeddingProviderID` field). The two fields under test —
// `embeddingProviderId` and `embeddingModelId` — are the single source
// of truth for "which embedding upstream do we call". Sibling scenario
// S-064 (semantic_cache_test.go) exercises the *runtime* effect of this
// pair (L2 hit when the embeddings cluster). S-082 narrowly covers the
// *admin* round-trip: change the pair, read it back, restore. We do
// not enable=true here — that path is owned by S-064 and requires a
// dimension probe.
//
// Eligibility model: a provider is "embedding-capable" if it has at
// least one Model row of `type=embedding`. The admin surface for
// discovery is `GET /api/admin/models/flat?type=embedding`, which
// returns `{data: [...], total: N}` with each row carrying `providerId`.
// (The list endpoint `GET /api/admin/providers` does not currently
// filter by capability; deriving via `models/flat?type=embedding` is
// the cleanest reflection of "embedding-capable" at this layer.) The
// local seed MUST ship at least one embedding-capable provider — if
// `models/flat?type=embedding` returns zero rows, that is a seed
// regression and the scenario t.Fatalfs rather than silently skipping.
//
// Hard-fail surfaces (per E86 hardening — no skip paths):
//   - GET /api/admin/semantic-cache/config returns 404 → admin surface
//     not mounted; this is a build/wiring regression, not an env quirk.
//   - models/flat returns zero embedding rows → seed regression.
//   - exactly one embedding-capable provider → still meaningful: write
//     the SAME pair back to confirm the PUT contract round-trips.
//
// Cleanup contract: register the restore PUT BEFORE issuing the
// mutating PUT. The Cleanup helper runs handlers in LIFO order on test
// exit (success OR failure), so a panic between the register and the
// PUT body still leaves fleet state untouched, and a panic after the
// PUT but before manual restore is recovered automatically.
//
// Probe-driven dimension (2026-05-22):
//
// The dev seed ships bogus provider credentials (sk-bogus…, fake Gemini
// keys, …) so a real upstream embedding call to api.openai.com /
// generativelanguage.googleapis.com always returns 401/403 and the
// probe responds `{ok:false, error:"..."}` — which would leave the
// scenario falling back to a pre-existing dimension or a synthetic
// 1536 default. That is not a real verification of the probe path.
//
// To exercise the probe end-to-end without depending on real upstream
// credentials, this scenario reuses S-067's pattern: stand up an
// in-process httptest.Server that mimics the OpenAI embeddings JSON
// envelope (1536-dim float32 vector), PUT the target provider's
// `baseUrl` to point at the mock for the duration of the test, then
// fire POST /api/admin/providers/:id/embedding-probe. Because the
// probe handler reads Provider.baseUrl from the CP store at call time
// (embedding_probe.go:51-53 + line 108), no Hub→AI-GW propagation is
// required — the probe will hit the mock immediately after the PUT.
//
// Assertions:
//  1. Read current config (200 + decodable JSON). Capture original
//     embeddingProviderId / embeddingModelId / threshold for restore.
//  2. List embedding-capable providers via models/flat. Skip if zero.
//  3. Pick a target (provider, model) pair: prefer a pair that differs
//     from the current config; otherwise fall back to the same pair
//     (single-provider environments) to verify the round-trip contract.
//  4. Register a restore-cleanup that PUTs the original pair back.
//  5. Stand up mock embedding server, repoint target Provider.baseUrl
//     at the mock, register a restore-cleanup for the original baseUrl.
//  6. POST /api/admin/providers/:id/embedding-probe and HARD-assert
//     `ok == true && dimension == 1536`. No fallback path.
//  7. PUT new pair (with probe-discovered dimension) → 200.
//  8. GET config again → embeddingProviderId and embeddingModelId
//     match the values just written.
func TestS082_EmbeddingProviderConfig(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// 1. Read current config.
	preStatus, preBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"GET", "/api/admin/semantic-cache/config", nil)
	if err != nil {
		t.Fatalf("GET semantic-cache/config: %v", err)
	}
	if preStatus != 200 {
		t.Fatalf("GET semantic-cache/config: status %d body=%q (404 here = admin surface not mounted; that is a build/wiring regression — fail hard, do not skip)",
			preStatus, truncate(preBody, 300))
	}

	// Decode only the writable subset we care about for restore. The
	// server-managed fields (id, updatedAt, updatedBy, embeddingFingerprint)
	// are deliberately not echoed back on PUT.
	var preCfg struct {
		EmbeddingProviderID *string  `json:"embeddingProviderId"`
		EmbeddingModelID    *string  `json:"embeddingModelId"`
		EmbeddingDimension  *int     `json:"embeddingDimension"`
		Enabled             bool     `json:"enabled"`
		Threshold           *float64 `json:"threshold"`
		VaryBy              *string  `json:"varyBy"`
		EmbedStrategy       *string  `json:"embedStrategy"`
		AllowCrossModel     *bool    `json:"allowCrossModel"`
	}
	if err := json.Unmarshal(preBody, &preCfg); err != nil {
		t.Fatalf("decode semantic-cache singleton: %v (body=%q)", err, truncate(preBody, 400))
	}

	// 2. Discover embedding-capable providers via the flat models list.
	//    `models/flat?type=embedding` returns the minimum set of models
	//    we'd accept as a valid pair to write back; each row carries
	//    `providerId`, which is what semantic-cache config stores.
	embStatus, embBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"GET", "/api/admin/models/flat?type=embedding&enabled=true&limit=100", nil)
	if err != nil {
		t.Fatalf("GET models/flat?type=embedding: %v", err)
	}
	if embStatus != 200 {
		t.Fatalf("GET models/flat?type=embedding: status %d body=%q",
			embStatus, truncate(embBody, 300))
	}
	var embList struct {
		Data []struct {
			ID         string `json:"id"`
			Code       string `json:"code"`
			Name       string `json:"name"`
			ProviderID string `json:"providerId"`
			Type       string `json:"type"`
		} `json:"data"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(embBody, &embList); err != nil {
		t.Fatalf("decode models/flat: %v (body=%q)", err, truncate(embBody, 400))
	}
	if len(embList.Data) == 0 {
		t.Fatalf("S-082 requires ≥1 embedding-capable provider configured at this target (models/flat returned 0 type=embedding rows) — local seed regression, fail hard")
	}

	// 3. Pick the target (provider, model) pair.
	//
	//    Preference order:
	//      a. A row whose (providerId, id) differs from the current
	//         (embeddingProviderId, embeddingModelId) — proves the PUT
	//         actually changes state across distinct rows.
	//      b. Failing that (single embedding model in the catalog, or
	//         the current pair is the only row), the FIRST row — proves
	//         the PUT contract round-trips even when re-writing the same
	//         pair (still asserts the write path is wired).
	type targetPair struct {
		ProviderID string
		ModelID    string
		ModelCode  string
	}
	var target targetPair
	for _, m := range embList.Data {
		if m.Type != "embedding" {
			continue
		}
		if preCfg.EmbeddingProviderID != nil && *preCfg.EmbeddingProviderID == m.ProviderID &&
			preCfg.EmbeddingModelID != nil && *preCfg.EmbeddingModelID == m.ID {
			continue
		}
		target = targetPair{ProviderID: m.ProviderID, ModelID: m.ID, ModelCode: m.Code}
		break
	}
	if target.ProviderID == "" {
		// Single embedding row in catalog OR every row matches the
		// current pair — fall back to the first row.
		first := embList.Data[0]
		target = targetPair{ProviderID: first.ProviderID, ModelID: first.ID, ModelCode: first.Code}
	}

	// 4. Register restore-cleanup BEFORE the mutating PUT. LIFO order +
	//    run-on-failure semantics ensure fleet state is restored even
	//    if the test body panics or t.Fatalf fires below.
	//
	//    Restore strategy: rebuild the PUT body from the GET-decoded
	//    subset. We deliberately do not re-PUT the raw GET bytes because
	//    GET includes server-managed read-only fields (updatedAt,
	//    embeddingFingerprint) that PUT rejects or ignores; rebuilding
	//    keeps the cleanup robust against future read-only additions.
	sc.Cleanup.Register("RestoreSemanticCacheConfigEmbeddingPair", func() error {
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
		if preCfg.VaryBy != nil {
			restore["varyBy"] = *preCfg.VaryBy
		}
		if preCfg.EmbedStrategy != nil {
			restore["embedStrategy"] = *preCfg.EmbedStrategy
		}
		if preCfg.AllowCrossModel != nil {
			restore["allowCrossModel"] = *preCfg.AllowCrossModel
		}
		body, err := json.Marshal(restore)
		if err != nil {
			return fmt.Errorf("marshal restore body: %w", err)
		}
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

	// 5. Stand up the mock embedding server and repoint the target
	//    Provider's baseUrl at it. This guarantees the probe in step 6
	//    hits a JSON-shape-correct upstream regardless of whether the
	//    seeded provider credential is real.
	//
	//    The mock is reused verbatim from S-067 (startMockEmbeddingServer
	//    in cache_prewarm_test.go) — same OpenAI embeddings envelope,
	//    same 1536-dim float vector. Both scenarios stay in lockstep so
	//    a future mock-shape change updates the single source.
	//
	//    Register restore-cleanup BEFORE the PUT so an aborted run still
	//    restores the original baseUrl.
	mock := startMockEmbeddingServer()
	defer mock.Close()
	t.Logf("S-082 mock embedding server at %s", mock.URL)

	// Capture original baseUrl for the target provider so cleanup can
	// restore it. Use the same helper S-067 uses.
	provGetStatus, provGetBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"GET", "/api/admin/providers/"+target.ProviderID, nil)
	if err != nil {
		t.Fatalf("GET target provider %s: %v", target.ProviderID, err)
	}
	if provGetStatus != 200 {
		t.Fatalf("GET target provider %s: status=%d body=%q",
			target.ProviderID, provGetStatus, truncate(provGetBody, 200))
	}
	var provPre struct {
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal(provGetBody, &provPre); err != nil {
		t.Fatalf("decode target provider: %v (body=%q)", err, truncate(provGetBody, 200))
	}
	if provPre.BaseURL == "" {
		t.Fatalf("target provider %s has empty baseUrl in response", target.ProviderID)
	}
	origBaseURL := provPre.BaseURL

	// Restore baseUrl on cleanup. Registered BEFORE the mutating PUT so
	// a panic between this line and the PUT still rewinds correctly.
	sc.Cleanup.Register("RestoreTargetProviderBaseURL", func() error {
		restoreBody := mustMarshal(t, map[string]any{"baseUrl": origBaseURL})
		status, body, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			"PUT", "/api/admin/providers/"+target.ProviderID, restoreBody)
		if err != nil {
			return fmt.Errorf("restore provider baseUrl PUT: %w", err)
		}
		if status != 200 {
			return fmt.Errorf("restore provider baseUrl PUT: status=%d body=%q",
				status, truncate(body, 200))
		}
		return nil
	})

	// Repoint baseUrl at the mock.
	repointBody := mustMarshal(t, map[string]any{"baseUrl": mock.URL})
	repointStatus, repointResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"PUT", "/api/admin/providers/"+target.ProviderID, repointBody)
	if err != nil {
		t.Fatalf("PUT target provider %s baseUrl=mock: %v", target.ProviderID, err)
	}
	if repointStatus != 200 {
		t.Fatalf("PUT target provider %s baseUrl=mock: status=%d body=%q",
			target.ProviderID, repointStatus, truncate(repointResp, 300))
	}
	t.Logf("S-082 repointed provider %s baseUrl: %s → %s",
		target.ProviderID, origBaseURL, mock.URL)

	// 6. Probe the target provider. The probe handler reads
	//    Provider.baseUrl from the CP store at call time
	//    (embedding_probe.go ProviderEmbeddingProbe → GetProvider →
	//    forwardEmbeddingProbe), so no Hub→AI-GW propagation is needed
	//    — the mock URL we just PUT is the URL the probe will hit.
	//
	//    HARD assertion: ok=true AND dimension=1536. No fallback to
	//    pre-existing dimension. No synthetic 1536 default. If the
	//    probe returns ok=false or a different dimension, the test
	//    fails — the entire point of S-082 is to verify the probe
	//    path actually works end-to-end through the admin surface.
	probePath := "/api/admin/providers/" + target.ProviderID + "/embedding-probe"
	probeStatus, probeBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"POST", probePath, []byte("{}"))
	if err != nil {
		t.Fatalf("POST embedding-probe: %v", err)
	}
	if probeStatus != 200 {
		t.Fatalf("POST embedding-probe: status=%d body=%q", probeStatus, truncate(probeBody, 400))
	}
	var probeResp struct {
		OK        bool    `json:"ok"`
		Dimension int     `json:"dimension"`
		ProviderID string `json:"providerId"`
		ModelID   string  `json:"modelId"`
		Error     string  `json:"error,omitempty"`
	}
	if err := json.Unmarshal(probeBody, &probeResp); err != nil {
		t.Fatalf("decode embedding-probe response: %v (body=%q)", err, truncate(probeBody, 400))
	}
	if !probeResp.OK {
		t.Fatalf("embedding-probe returned ok=false (error=%q dimension=%d body=%q) — mock server should have made the probe succeed; check that PUT provider baseUrl landed and CP store reads it back",
			probeResp.Error, probeResp.Dimension, truncate(probeBody, 400))
	}
	if probeResp.Dimension != 1536 {
		t.Fatalf("embedding-probe dimension=%d (want 1536 — the mock embedding server returns a 1536-dim vector); body=%q",
			probeResp.Dimension, truncate(probeBody, 400))
	}
	dimension := probeResp.Dimension
	t.Logf("S-082 probe OK: ok=true dimension=%d provider=%s model=%s",
		dimension, target.ProviderID, target.ModelID)

	// 7. PUT the new (provider, model, dimension) triple. We hold
	//    `enabled=false` deliberately: actually enabling fleet-wide L2
	//    is the runtime path covered by S-064.
	putPayload := map[string]any{
		"embeddingProviderId": target.ProviderID,
		"embeddingModelId":    target.ModelID,
		"embeddingDimension":  dimension,
		"enabled":             false,
	}
	// Preserve any pre-existing tuning so the PUT does not silently
	// flatten them. The handler normalizes unknown / out-of-range
	// values to schema defaults, but echoing what we read back is the
	// minimum-surprise contract.
	if preCfg.Threshold != nil {
		putPayload["threshold"] = *preCfg.Threshold
	}
	if preCfg.VaryBy != nil {
		putPayload["varyBy"] = *preCfg.VaryBy
	}
	if preCfg.EmbedStrategy != nil {
		putPayload["embedStrategy"] = *preCfg.EmbedStrategy
	}
	if preCfg.AllowCrossModel != nil {
		putPayload["allowCrossModel"] = *preCfg.AllowCrossModel
	}
	putBody := mustMarshal(t, putPayload)
	putStatus, putRespBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"PUT", "/api/admin/semantic-cache/config", putBody)
	if err != nil {
		t.Fatalf("PUT semantic-cache/config: %v", err)
	}
	if putStatus != 200 {
		t.Fatalf("PUT semantic-cache/config: status %d body=%q (payload=%q)",
			putStatus, truncate(putRespBody, 400), truncate(putBody, 200))
	}

	// 8. Read it back and verify the change persisted.
	postStatus, postBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		"GET", "/api/admin/semantic-cache/config", nil)
	if err != nil {
		t.Fatalf("GET semantic-cache/config (post): %v", err)
	}
	if postStatus != 200 {
		t.Fatalf("GET semantic-cache/config (post): status %d body=%q",
			postStatus, truncate(postBody, 300))
	}
	var postCfg struct {
		EmbeddingProviderID *string `json:"embeddingProviderId"`
		EmbeddingModelID    *string `json:"embeddingModelId"`
		EmbeddingDimension  *int    `json:"embeddingDimension"`
	}
	if err := json.Unmarshal(postBody, &postCfg); err != nil {
		t.Fatalf("decode post semantic-cache singleton: %v (body=%q)",
			err, truncate(postBody, 400))
	}
	if postCfg.EmbeddingProviderID == nil || *postCfg.EmbeddingProviderID != target.ProviderID {
		got := "<nil>"
		if postCfg.EmbeddingProviderID != nil {
			got = *postCfg.EmbeddingProviderID
		}
		t.Errorf("embeddingProviderId did not persist: want=%q got=%q", target.ProviderID, got)
	}
	if postCfg.EmbeddingModelID == nil || *postCfg.EmbeddingModelID != target.ModelID {
		got := "<nil>"
		if postCfg.EmbeddingModelID != nil {
			got = *postCfg.EmbeddingModelID
		}
		t.Errorf("embeddingModelId did not persist: want=%q got=%q", target.ModelID, got)
	}
	if postCfg.EmbeddingDimension == nil || *postCfg.EmbeddingDimension != dimension {
		got := -1
		if postCfg.EmbeddingDimension != nil {
			got = *postCfg.EmbeddingDimension
		}
		t.Errorf("embeddingDimension did not persist: want=%d got=%d", dimension, got)
	}
	t.Logf("S-082 OK: probe-driven dimension=%d; wrote embeddingProviderId=%s embeddingModelId=%s (code=%s); round-trip verified",
		dimension, target.ProviderID, target.ModelID, target.ModelCode)
}
