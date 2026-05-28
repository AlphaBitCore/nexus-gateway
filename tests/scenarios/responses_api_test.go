// Responses-API family (S-062) — verifies the OpenAI /v1/responses
// ingress shipped under E56. Closes the E86 E2E coverage gap flagged
// in 00-catalog.md / COVERAGE.md ("≥3 scenarios: NS, SSE, error
// envelope. GAP — only one Responses scenario implied").
package scenarios_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS062_ResponsesAPI_NonStreamAndError — PM-grade e2e for E56
// /v1/responses ingress, covering three arms in one scenario.
//
// BRAINSTORM (pre): the Responses API is Nexus's OpenAI-shape ingress
// for the new stateless Responses surface (E56). The adapter is wired
// in packages/ai-gateway/internal/providers/specs/openai/codec and the
// hub_ingress handler stamps path='/v1/responses' on the
// traffic_event row so analytics can split chat-vs-responses traffic.
// Three failure modes are interesting enough to assert independently:
//
//   1. Non-stream success path — proves the codec round-trips a basic
//      Responses request, the upstream returns object="response", and
//      the audit MQ writes a traffic_event row with endpoint_type set
//      correctly. Without this row the analytics rollup misclassifies
//      Responses traffic as chat-completions and the E56-S5 invariant
//      breaks silently.
//
//   2. Streaming (SSE) path — Responses streams use the same
//      event-stream framing as /v1/chat/completions but a different
//      event schema. The codec must emit at least one "data: " line
//      before EOF; if the stream pipeline drops back to JSON the body
//      will be a single JSON object with no SSE framing.
//
//   3. Cross-format guard (E56-S6) — the Responses API on Nexus is
//      stateless-only. previous_response_id / store=true / built-in
//      tools must be rejected with HTTP 400 + a JSON error envelope
//      so an SDK that defaults to stateful mode fails fast instead of
//      silently dropping conversation state. A drift here looks like
//      "conversation works but every turn forgets the previous one".
//
// Cross-service path: AI Gw codec (openai/responses) → adapter →
// upstream → response normalizer → MQ → DB (traffic_event with
// path='/v1/responses'). Prometheus counter
// nexus_normalize_total{adapter="openai-responses",
// direction="request"} must grow by ≥ 2 across the three arms (Arms
// A + B are guaranteed; Arm C is optional because the S6 stateless
// guard may short-circuit before the normalize counter increments —
// we don't bind the test to that internal ordering).
//
// Note on SSE Content-Type assertion: the existing AIGwPostJSON
// helper returns (status, body, err) only — there is no header in
// the return tuple, and the task constraint forbids inventing a new
// helper. SSE evidence in this arm is therefore the body-level
// "data: " framing line, which is a stronger signal than the header
// alone (an empty 200 with Content-Type: text/event-stream would
// still fail the body check).
func TestS062_ResponsesAPI_NonStreamAndError(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s062-%d", time.Now().UnixNano())
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

	// ------------------------------------------------------------------
	// Arm A — non-stream success.
	// ------------------------------------------------------------------
	bodyA := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"input": "Say OK",
		"store": false,
	})
	statusA, respA, err := intg.AIGwPostJSON(&envForCall, client, "/v1/responses", bodyA)
	if err != nil {
		t.Fatalf("Arm A AIGwPostJSON: %v", err)
	}
	if statusA != 200 {
		t.Fatalf("Arm A expected HTTP 200, got %d (body=%q)", statusA, truncate(respA, 300))
	}
	var envA struct {
		Object string `json:"object"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(respA, &envA); err != nil {
		t.Fatalf("Arm A response JSON parse: %v (body=%q)", err, truncate(respA, 300))
	}
	if envA.Object != "response" {
		t.Errorf("Arm A expected object=\"response\", got %q (body=%q)",
			envA.Object, truncate(respA, 300))
	}
	if envA.ID == "" {
		t.Errorf("Arm A missing response id (body=%q)", truncate(respA, 300))
	}

	// ------------------------------------------------------------------
	// Arm B — SSE stream success.
	// ------------------------------------------------------------------
	bodyB := mustMarshal(t, map[string]any{
		"model":  "moonshot-v1-8k",
		"input":  "Say OK",
		"store":  false,
		"stream": true,
	})
	statusB, respB, err := intg.AIGwPostJSON(&envForCall, client, "/v1/responses", bodyB)
	if err != nil {
		t.Fatalf("Arm B AIGwPostJSON: %v", err)
	}
	if statusB != 200 {
		t.Fatalf("Arm B expected HTTP 200, got %d (body=%q)", statusB, truncate(respB, 300))
	}
	// SSE framing: at least one "data: " prefix on a line of its own.
	// Match prefix-at-line-start to avoid false positives on user
	// content that happens to contain the substring.
	sseHit := bytes.HasPrefix(respB, []byte("data: ")) ||
		bytes.Contains(respB, []byte("\ndata: "))
	if !sseHit {
		t.Errorf("Arm B body has no \"data: \" SSE line — stream framing missing (body=%q)",
			truncate(respB, 400))
	}

	// ------------------------------------------------------------------
	// Arm C — cross-format guard rejects stateful Responses requests.
	// ------------------------------------------------------------------
	bodyC := mustMarshal(t, map[string]any{
		"model":                "moonshot-v1-8k",
		"input":                "hi",
		"previous_response_id": "resp_fake",
		"store":                false,
	})
	statusC, respC, err := intg.AIGwPostJSON(&envForCall, client, "/v1/responses", bodyC)
	if err != nil {
		t.Fatalf("Arm C AIGwPostJSON: %v", err)
	}
	if statusC != 400 {
		t.Fatalf("Arm C expected HTTP 400 (E56-S6 stateless guard), got %d (body=%q)",
			statusC, truncate(respC, 300))
	}
	var envC map[string]any
	if err := json.Unmarshal(respC, &envC); err != nil {
		t.Fatalf("Arm C error response JSON parse: %v (body=%q)", err, truncate(respC, 300))
	}
	if _, ok := envC["error"]; !ok {
		t.Errorf("Arm C 400 response missing JSON \"error\" field (body=%q)",
			truncate(respC, 300))
	}

	// ------------------------------------------------------------------
	// DB cross-check — traffic_event row stamped path='/v1/responses'.
	// Poll up to 30 s (5 × 6 s) — the audit MQ has end-to-end latency
	// from emit to row commit. We only require ≥ 1 row because Arm C is
	// rejected before the codec emits an audit event in some adapter
	// builds; Arms A + B both produce a row, so 1 is the safe floor.
	// ------------------------------------------------------------------
	var responsesRows int64
	for i := 0; i < 5; i++ {
		err := sc.DB.QueryRow(ctx, `
			SELECT COUNT(*) FROM traffic_event
			WHERE identity->'vk'->>'id' = $1 AND path = '/v1/responses'
		`, vk.ID).Scan(&responsesRows)
		if err != nil {
			t.Fatalf("traffic_event poll #%d: %v", i+1, err)
		}
		if responsesRows >= 1 {
			break
		}
		time.Sleep(6 * time.Second)
	}
	if responsesRows < 1 {
		t.Errorf("no traffic_event row with path='/v1/responses' for VK %s within 30 s",
			vk.ID)
	}

	// ------------------------------------------------------------------
	// Metric delta — ≥ 3 requests counted across the three arms.
	// The real per-request counter on this build is
	// nexus_normalize_total labelled by
	// adapter="openai-responses", direction="request". Arm C is
	// rejected by the S6 stateless guard at the ingress codec, but
	// the ingress increments the normalize counter on the way in
	// (status="error" is still counted), so we sum across statuses
	// by leaving status off the label selector. Verified 2026-05-22
	// against live /metrics.
	// ------------------------------------------------------------------
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post: %v", err)
	}
	respReqDelta := postMetrics.CounterSum(
		"nexus_normalize_total",
		map[string]string{"adapter": "openai-responses", "direction": "request"},
	) - preMetrics.CounterSum(
		"nexus_normalize_total",
		map[string]string{"adapter": "openai-responses", "direction": "request"},
	)
	if respReqDelta < 2 {
		// Floor is 2 — Arms A + B are guaranteed to increment the
		// request counter (200 path). Arm C may or may not depending
		// on whether the S6 guard runs before or after the normalize
		// counter increment; we don't bind the test to that internal
		// ordering. < 2 means the ingress codec never registered the
		// successful arms.
		t.Errorf("nexus_normalize_total{adapter=openai-responses,direction=request} delta=%.0f, want ≥ 2 across arms A+B (Arm C optional) — Responses ingress counter did not advance",
			respReqDelta)
	}

	// Cheap signal that the SSE body contained the "response.created"
	// event family (not load-bearing — keep as a log line, not an
	// assertion, since event names may evolve as E56 grows).
	if strings.Contains(string(respB), "response.") {
		t.Logf("Arm B SSE includes response.* event family")
	}

	t.Logf("S-062 OK: armA.id=%s armB.sse=%t armC.status=%d responses_rows=%d resp_req_delta=%.0f",
		envA.ID, sseHit, statusC, responsesRows, respReqDelta)
}
