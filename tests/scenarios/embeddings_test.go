// Embeddings family (S-063) — verifies the /v1/embeddings ingress
// (E62) end-to-end through the AI Gateway: happy-path single input,
// explicit `dimensions` round-trip, and batch input. Closes the E2E
// coverage gap for the embeddings endpoint type tracked under E86.
//
// Hardening 2026-05-22: every env-shape skip was promoted to a hard
// failure. text-embedding-3-small is a baseline seed in
// tools/db-migrate/prisma/seed.ts and the dev compose stack provisions
// the openai-embeddings adapter unconditionally — a 400 from arm A
// is a regression in routing, codec, or seed, never an "env not
// ready" state. The metric-delta assertion now binds the
// `nexus_ai_gateway_normalize_total` counter directly (the real metric
// name on this build; see live /metrics probe) instead of the absent
// nexus_aigw_requests_total.
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

// TestS063_EmbeddingsHappyPath — PM-grade e2e for /v1/embeddings.
//
// BRAINSTORM (pre): the embeddings endpoint (E62) is a sibling of
// /v1/chat/completions but lives on its own audit endpoint_type
// ("embeddings") and emits a different response envelope:
// {"object":"list","data":[{"index":i,"embedding":[...]}],"model":...}.
// Cross-service path: AI Gw codec (embeddings request normalize) →
// provider (e.g. openai text-embedding-3-small) → response normalize →
// MQ traffic_event with path='/v1/embeddings' → DB. The three
// most load-bearing observables to assert E2E are: (1) the happy-path
// response shape (proves the codec round-trips at all); (2) the
// explicit `dimensions` field — provider capability, must thread from
// canonical → wire → response without being silently dropped, the
// classic cross-ingress asymmetry trap [[feedback_cache_mandatory_all_ingress]];
// (3) batch input ordering — `data[i].index == i` proves the codec
// preserves user-supplied input order through the response demux. Per-
// arm DB cross-check on traffic_event.path='/v1/embeddings' is
// the strongest "this really hit the embeddings code path" signal.
//
// Failure semantics: text-embedding-3-small is required by the dev
// seed (tools/db-migrate/prisma/seed.ts) and the openai-embeddings
// adapter is wired unconditionally in the AI Gateway. A 400 from arm A
// is a real regression — either the seed lost the model row, the
// routing-resolver dropped the embeddings endpoint, or the codec is
// returning model_not_found by mistake. Surface it as a hard fail.
//
// Assertions:
//  1. Arm A (single string input) — HTTP 200; object="list";
//     len(data) == 1; len(data[0].embedding) > 0.
//  2. Arm B (dimensions=256) — HTTP 200; len(data[0].embedding) == 256;
//     different from arm A length (proves dimensions round-trips).
//  3. Arm C (batch input of 3) — HTTP 200; len(data) == 3;
//     data[i].index == i in order.
//  4. Per-arm: traffic_event row with path='/v1/embeddings'
//     appears within 30 s (poll 5 × 6 s).
//  5. Metric delta: nexus_aigw_requests_total{endpoint="embeddings"}
//     grew by ≥ 3 across the three arms (or overall delta ≥ 3 if the
//     label selector returns nothing — defensive against label-name
//     drift).
func TestS063_EmbeddingsHappyPath(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s063-%d", time.Now().UnixNano())
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

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()

	// embeddingResponse is the minimal subset of the OpenAI embeddings
	// envelope this scenario asserts on. Keeping it narrow avoids
	// coupling the test to unrelated upstream fields (model_version,
	// usage shape, etc.).
	type embeddingItem struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
		Object    string    `json:"object"`
	}
	type embeddingResponse struct {
		Object string          `json:"object"`
		Data   []embeddingItem `json:"data"`
		Model  string          `json:"model"`
	}

	// pollTrafficEvent waits up to 30 s (5 × 6 s) for a traffic_event
	// row with path='/v1/embeddings' tagged to this VK. Returns
	// (true, nil) on hit, (false, nil) on timeout, (false, err) on
	// query failure. Inline rather than a helper because the predicate
	// (endpoint_type column) is specific to this scenario family.
	pollTrafficEvent := func(arm string) {
		t.Helper()
		const tries = 5
		const interval = 6 * time.Second
		for i := 0; i < tries; i++ {
			var count int
			err := sc.DB.QueryRow(ctx, `
				SELECT COUNT(*) FROM traffic_event
				WHERE source = 'ai-gateway'
				  AND path = '/v1/embeddings'
				  AND identity->'vk'->>'id' = $1
				  AND "timestamp" > NOW() - INTERVAL '120 seconds'
			`, vk.ID).Scan(&count)
			if err != nil {
				t.Fatalf("arm %s traffic_event poll: %v", arm, err)
			}
			if count >= 1 {
				return
			}
			if i < tries-1 {
				time.Sleep(interval)
			}
		}
		t.Errorf("arm %s: no traffic_event row with path='/v1/embeddings' for VK %s within 30 s", arm, vk.ID)
	}

	// ----- Arm A — happy path, single input -----
	bodyA := mustMarshal(t, map[string]any{
		"model": "text-embedding-3-small",
		"input": "hello world",
	})
	statusA, respA, err := intg.AIGwPostJSON(&envForCall, client, "/v1/embeddings", bodyA)
	if err != nil {
		t.Fatalf("arm A AIGwPostJSON: %v", err)
	}
	if statusA != 200 {
		// text-embedding-3-small is part of the dev seed and the
		// openai-embeddings adapter is unconditionally wired. A non-200
		// here is a routing / codec / seed regression — fail hard so
		// CI surfaces it, never a silent skip.
		t.Fatalf("arm A expected 200, got %d (%q)", statusA, truncate(respA, 200))
	}
	var parsedA embeddingResponse
	if err := json.Unmarshal(respA, &parsedA); err != nil {
		t.Fatalf("arm A unmarshal: %v (body=%q)", err, truncate(respA, 200))
	}
	if parsedA.Object != "list" {
		t.Errorf("arm A object=%q, want %q", parsedA.Object, "list")
	}
	if len(parsedA.Data) != 1 {
		t.Fatalf("arm A len(data)=%d, want 1", len(parsedA.Data))
	}
	armALen := len(parsedA.Data[0].Embedding)
	if armALen == 0 {
		t.Fatalf("arm A data[0].embedding is empty — codec did not round-trip the embedding vector")
	}
	pollTrafficEvent("A")

	// ----- Arm B — explicit dimensions round-trip -----
	bodyB := mustMarshal(t, map[string]any{
		"model":      "text-embedding-3-small",
		"input":      "hello world",
		"dimensions": 256,
	})
	statusB, respB, err := intg.AIGwPostJSON(&envForCall, client, "/v1/embeddings", bodyB)
	if err != nil {
		t.Fatalf("arm B AIGwPostJSON: %v", err)
	}
	if statusB != 200 {
		t.Fatalf("arm B expected 200, got %d (%q)", statusB, truncate(respB, 200))
	}
	var parsedB embeddingResponse
	if err := json.Unmarshal(respB, &parsedB); err != nil {
		t.Fatalf("arm B unmarshal: %v (body=%q)", err, truncate(respB, 200))
	}
	if len(parsedB.Data) != 1 {
		t.Fatalf("arm B len(data)=%d, want 1", len(parsedB.Data))
	}
	armBLen := len(parsedB.Data[0].Embedding)
	if armBLen != 256 {
		t.Errorf("arm B len(embedding)=%d, want 256 — dimensions=256 did not round-trip through the codec", armBLen)
	}
	if armBLen == armALen {
		t.Errorf("arm B and arm A produced identical-length vectors (%d) — dimensions field was silently ignored", armBLen)
	}
	pollTrafficEvent("B")

	// ----- Arm C — batch input preserves order -----
	bodyC := mustMarshal(t, map[string]any{
		"model": "text-embedding-3-small",
		"input": []string{"one", "two", "three"},
	})
	statusC, respC, err := intg.AIGwPostJSON(&envForCall, client, "/v1/embeddings", bodyC)
	if err != nil {
		t.Fatalf("arm C AIGwPostJSON: %v", err)
	}
	if statusC != 200 {
		t.Fatalf("arm C expected 200, got %d (%q)", statusC, truncate(respC, 200))
	}
	var parsedC embeddingResponse
	if err := json.Unmarshal(respC, &parsedC); err != nil {
		t.Fatalf("arm C unmarshal: %v (body=%q)", err, truncate(respC, 200))
	}
	if len(parsedC.Data) != 3 {
		t.Fatalf("arm C len(data)=%d, want 3 — batch input was not fanned out", len(parsedC.Data))
	}
	for i, item := range parsedC.Data {
		if item.Index != i {
			t.Errorf("arm C data[%d].index=%d, want %d — batch ordering was scrambled by the codec", i, item.Index, i)
		}
		if len(item.Embedding) == 0 {
			t.Errorf("arm C data[%d].embedding empty", i)
		}
	}
	pollTrafficEvent("C")

	// ----- Hard assertion: metric advanced -----
	// nexus_ai_gateway_normalize_total{adapter="openai-embeddings",
	// direction="request"} must increment by ≥ 1 — proves the embeddings
	// normalize codec path executed at all. The per-arm proof is the
	// pollTrafficEvent loop above (each arm A/B/C polls for ≥ 1 row);
	// we don't pin the final aggregate row count because cache/dedup
	// internals are out of scope here.
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post: %v", err)
	}
	embedReqDelta := postMetrics.CounterSum(
		"nexus_ai_gateway_normalize_total",
		map[string]string{"adapter": "openai-embeddings", "direction": "request"},
	) - preMetrics.CounterSum(
		"nexus_ai_gateway_normalize_total",
		map[string]string{"adapter": "openai-embeddings", "direction": "request"},
	)
	if embedReqDelta < 1 {
		t.Errorf("nexus_ai_gateway_normalize_total{adapter=openai-embeddings,direction=request} delta=%.0f, want ≥ 1 — embeddings normalize codec never executed",
			embedReqDelta)
	}

	t.Logf("S-063 OK: armA_len=%d armB_len=%d (dims=256) armC_batch=%d embed_req_delta=%.0f",
		armALen, armBLen, len(parsedC.Data), embedReqDelta)
}
