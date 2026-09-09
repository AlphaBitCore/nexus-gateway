// AI-Guard compliance-webhook family (S-086) — verifies the
// /v1/ai-guard/compliance-webhook ingress. This endpoint is the
// webhook-forward sink that hook rows with action=webhook-forward POST
// into; the AI Gateway evaluates the payload via the configured AIGuard
// classifier and returns a webhook-shape decision envelope
// (decision ∈ {APPROVE, REJECT_HARD, REJECT_SOFT, MODIFY, ABSTAIN}).
//
// Handler: packages/ai-gateway/internal/ingress/proxy/classify/classify.go
//
//	(ServeComplianceWebhookHTTP)
//
// Wiring : packages/ai-gateway/cmd/ai-gateway/wiring/thingclient.go
//
//	(mountAIGuardRoutes — the route sits BEHIND rstokenauth, which
//	reads the shared internal secret from the X-RS-Token HEADER.
//	Any Authorization bearer, VK or service token, gets 401
//	RS_TOKEN_REQUIRED. Verified live both ways.
//	sse-streaming-compliance-architecture.md documents the same
//	contract from the caller side: webhook-forward injects
//	X-RS-Token per request, and only for this exact path against a
//	trusted base.)
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

// TestS086_AIGuardComplianceWebhook — PM-grade e2e for the
// /v1/ai-guard/compliance-webhook ingress. Three arms cover the three
// distinct failure modes a webhook-forward integration cares about:
//
//  1. Happy path — well-formed ComplianceWebhookRequest (per the
//     OpenAPI schema): stage/method/path/targetHost/model/ingressType +
//     normalizedContent. The handler always returns a structured JSON
//     decision envelope; HTTP status is 200 when the classifier ran
//     (regardless of which decision it produced) or 503 when the
//     AIGuard backend is unavailable (acceptable in CI where the
//     backend is often unconfigured). Both responses MUST carry a JSON
//     body with a known shape — status==200 → ComplianceWebhookResponse
//     with `decision` in the documented enum; status==503 → ErrorBody
//     with `error`.
//
//  2. Malformed body — POST `{` (invalid JSON). Handler responds
//     HTTP 400 with ErrorBody{error:"malformed_json"}. This proves the
//     decode-error branch is wired and prevents a regression where a
//     panic-on-decode would crash the gateway under a malformed
//     webhook payload from a misbehaving compliance-proxy build.
//
//  3. Auth gate — POST with NO X-RS-Token. The expected behaviour is a
//     401: an internal classification surface must stay closed to
//     unauthenticated callers, and reaching the handler without the
//     header would mean the gate was removed or bypassed. Why this arm
//     asserts the refusal rather than the acceptance is argued at the
//     arm itself.
//
// Metric: nexus_requests_total — per the same labelling
// convention used by S-062, this counter ticks on every ingress request
// regardless of the eventual HTTP status. We assert delta ≥ 3 across
// the three arms via CounterSum (label-free, sums all label
// permutations) so the test is robust whether or not the counter
// carries a `path` label in this build.
func TestS086_AIGuardComplianceWebhook(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	// Arms 1 + 2 authenticate with X-RS-Token alone — rstokenauth reads the
	// shared secret from that header and nothing else. Arm 3 omits it and sends
	// a Bearer instead, which is what makes its 401 a statement about the
	// missing shared secret rather than about an unauthenticated request.
	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}
	vkName := fmt.Sprintf("s086-%d", time.Now().UnixNano())
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
	// Arm 1 — happy path, well-formed webhook payload.
	// ------------------------------------------------------------------
	// Shape matches ComplianceWebhookRequest in the OpenAPI spec
	// verbatim. normalizedContent is the canonical content channel the
	// handler prefers (webhookPayloadContent: normalizedContent → joined
	// text). The model + ingressType fields populate the AIGuard request
	// context so the classifier can pick the right detector profile.
	armABody := mustMarshal(t, map[string]any{
		"stage":       "request",
		"method":      "POST",
		"path":        "/v1/chat/completions",
		"targetHost":  "api.openai.com",
		"sourceIP":    "127.0.0.1",
		"bodySize":    128,
		"contentType": "application/json",
		"model":       "gpt-4o-mini",
		"ingressType": "chat",
		"normalizedContent": []string{
			fmt.Sprintf("user: Hello, please summarise this document. nonce=%d", time.Now().UnixNano()),
		},
	})
	// rstokenauth reads the shared internal secret from the X-RS-Token HEADER, so a
	// bearer — VK or service token alike — gets 401 RS_TOKEN_REQUIRED. Verified live
	// both ways: bearer 401, X-RS-Token 200 with decision=APPROVE.
	statusA, bodyA, err := intg.AIGwPostRSToken(&envForCall, client,
		"/v1/ai-guard/compliance-webhook", armABody)
	if err != nil {
		t.Fatalf("Arm A AIGwPostJSON: %v", err)
	}
	switch statusA {
	case 200:
		var parsed struct {
			Decision   string `json:"decision"`
			Reason     string `json:"reason"`
			ReasonCode string `json:"reasonCode"`
		}
		if jsonErr := json.Unmarshal(bodyA, &parsed); jsonErr != nil {
			t.Fatalf("Arm A: 200 body not JSON: %v (body=%q)",
				jsonErr, truncate(bodyA, 200))
		}
		// Documented enum per OpenAPI:
		//   APPROVE | REJECT_HARD | REJECT_SOFT | MODIFY | ABSTAIN
		validDecisions := map[string]bool{
			"APPROVE": true, "REJECT_HARD": true, "REJECT_SOFT": true,
			"MODIFY": true, "ABSTAIN": true,
		}
		if !validDecisions[parsed.Decision] {
			// Yaml-shape mismatch: if the backend's decision token
			// doesn't match the documented enum, fail loudly — the
			// OpenAPI yaml is the source of truth and any drift here
			// must surface as a regression in lockstep (update yaml +
			// gateway + this scenario together), not as a silent skip.
			t.Fatalf("S-086 happy-path decision %q not in documented enum {APPROVE,REJECT_HARD,REJECT_SOFT,MODIFY,ABSTAIN} (body=%q) — yaml + gateway are out of lockstep",
				parsed.Decision, truncate(bodyA, 200))
		}
		t.Logf("Arm A OK: status=200 decision=%s reason=%q reasonCode=%q",
			parsed.Decision, parsed.Reason, parsed.ReasonCode)
	case 503:
		// Backend unavailable is a legitimate happy-path outcome in CI
		// where the AIGuard external backend isn't wired. The contract
		// is that the body still parses as ErrorBody.
		var parsed struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if jsonErr := json.Unmarshal(bodyA, &parsed); jsonErr != nil {
			t.Fatalf("Arm A: 503 body not JSON: %v (body=%q)",
				jsonErr, truncate(bodyA, 200))
		}
		if parsed.Error == "" {
			t.Errorf("Arm A: 503 ErrorBody.error empty (body=%q)",
				truncate(bodyA, 200))
		}
		t.Logf("Arm A OK (backend unavailable): status=503 error=%s detail=%q",
			parsed.Error, parsed.Detail)
	default:
		// Anything else means the yaml-documented response shape
		// (200|400|500|503) and the backend's reality have drifted —
		// fail loudly so the lockstep update is forced into the same PR.
		t.Fatalf("S-086 happy-path returned unexpected status %d; yaml documents {200,400,500,503} — gateway and yaml are out of lockstep (body=%q)",
			statusA, truncate(bodyA, 200))
	}

	// ------------------------------------------------------------------
	// Arm 2 — malformed body. Single open brace is invalid JSON.
	// ------------------------------------------------------------------
	statusB, bodyB, err := intg.AIGwPostRSToken(&envForCall, client,
		"/v1/ai-guard/compliance-webhook", []byte("{"))
	if err != nil {
		t.Fatalf("Arm B AIGwPostJSON: %v", err)
	}
	if statusB != 400 {
		t.Errorf("Arm B: expected 400 for malformed JSON, got %d (body=%q)",
			statusB, truncate(bodyB, 200))
	}
	var armBParsed struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if jsonErr := json.Unmarshal(bodyB, &armBParsed); jsonErr != nil {
		t.Errorf("Arm B: 400 body not JSON: %v (body=%q)",
			jsonErr, truncate(bodyB, 200))
	}
	if armBParsed.Error == "" {
		t.Errorf("Arm B: ErrorBody.error empty for malformed JSON (body=%q)",
			truncate(bodyB, 200))
	}
	t.Logf("Arm B OK: status=%d error=%s", statusB, armBParsed.Error)

	// ------------------------------------------------------------------
	// Arm 3 — no-auth admittance. Confirms the endpoint accepts
	// unauthenticated POSTs (current contract; OpenAPI carries no
	// `security` block). We use a fresh Env clone with empty TestVK so
	// AIGwPostJSON sends "Authorization: Bearer " (empty token) — the
	// closest the helper can get to "no auth" without bypassing the
	// helper. A future hardening that adds rstokenauth.MiddlewareHTTP
	// to this route would surface here as a 401/403, immediately
	// flagging the contract change.
	// ------------------------------------------------------------------
	envNoAuth := *sc.Env
	envNoAuth.TestVK = "" // empty Bearer — exercises the no-auth contract
	armCBody := mustMarshal(t, map[string]any{
		"stage":       "request",
		"method":      "POST",
		"path":        "/v1/chat/completions",
		"targetHost":  "api.openai.com",
		"model":       "gpt-4o-mini",
		"ingressType": "chat",
		"normalizedContent": []string{
			fmt.Sprintf("user: simple no-auth probe. nonce=%d", time.Now().UnixNano()),
		},
	})
	statusC, bodyC, err := intg.AIGwPostJSON(&envNoAuth, client,
		"/v1/ai-guard/compliance-webhook", armCBody)
	if err != nil {
		t.Fatalf("Arm C AIGwPostJSON: %v", err)
	}
	// This arm asserts the REFUSAL, and the inversion is the point. It was written
	// to prove the route accepted anonymous POSTs, predicting in its own comment
	// that "a future hardening that adds rstokenauth.MiddlewareHTTP to this route
	// would surface here as a 401/403, immediately flagging the contract change".
	// The hardening shipped and the arm flagged it exactly as designed. The other
	// two ways to answer that flag are deleting a security assertion and re-opening
	// the route to satisfy a test; inverting the expectation is the third.
	switch statusC {
	case 401, 403:
		t.Logf("Arm C OK: anonymous POST correctly refused (status=%d, body=%q)",
			statusC, truncate(bodyC, 200))
	case 200, 503:
		t.Errorf("Arm C: the endpoint ACCEPTED an anonymous POST with status=%d — rstokenauth "+
			"gates this route on the X-RS-Token header, so reaching the handler without one means "+
			"the gate was removed or bypassed. An internal classification surface open to anyone "+
			"who can reach the port is a security regression (body=%q)",
			statusC, truncate(bodyC, 200))
	default:
		t.Errorf("Arm C: unexpected status=%d (want 401|403 — the route is rstokenauth-gated) (body=%q)",
			statusC, truncate(bodyC, 200))
	}

	// ------------------------------------------------------------------
	// Metric delta — /v1/ai-guard/compliance-webhook drives aiguard
	// classification metrics, not the general ai_gateway_requests_total
	// (verified empirically 2026-05-21 against /metrics). The classify
	// pipeline ticks nexus_aiguard_decisions_total (+ cache hit/miss for
	// the cached arm). Arm A is a fresh classify → decisions_total ≥ 1
	// + cache_writes_total ≥ 1. Arm B is malformed JSON, no classify
	// runs. Arm C is also a classify (cached or fresh). So decisions
	// delta ≥ 2 (arms A + C); cache_lookups (hits+misses) ≥ 2.
	// ------------------------------------------------------------------
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post: %v", err)
	}
	decisionsDelta := postMetrics.CounterSum("nexus_aiguard_decisions_total", nil) -
		preMetrics.CounterSum("nexus_aiguard_decisions_total", nil)
	cacheLookupsDelta := (postMetrics.CounterSum("nexus_aiguard_cache_hits_total", nil) +
		postMetrics.CounterSum("nexus_aiguard_cache_misses_total", nil)) -
		(preMetrics.CounterSum("nexus_aiguard_cache_hits_total", nil) +
			preMetrics.CounterSum("nexus_aiguard_cache_misses_total", nil))
	// Floor is 1, not 2. The old floor counted arms A AND C as classifications,
	// which only held while arm C reached the handler anonymously. Now that the
	// route is rstokenauth-gated, arm C is refused at the middleware and never
	// touches the classifier — so exactly one classification happens, and
	// expecting two would fail for the same reason the security gate exists.
	if decisionsDelta < 1 {
		t.Errorf("nexus_aiguard_decisions_total delta=%g (want ≥ 1 from arm A — arm C is refused "+
			"before the classifier, and arm B never decodes)", decisionsDelta)
	}
	if cacheLookupsDelta < 1 {
		t.Errorf("nexus_aiguard_cache_{hits,misses}_total delta=%g (want ≥ 1 — one classification, "+
			"so one cache lookup)", cacheLookupsDelta)
	}
	reqDelta := decisionsDelta // kept for the OK log line below

	t.Logf("S-086 OK: armA=%d armB=%d armC=%d req_delta=%.0f",
		statusA, statusB, statusC, reqDelta)
}
