// AI-Guard compliance-webhook family (S-086) — verifies the
// /v1/ai-guard/compliance-webhook ingress (E31-S2). This endpoint is
// the webhook-forward sink that hook rows with action=webhook-forward
// POST into; the AI Gateway evaluates the payload via the configured
// AIGuard classifier and returns a webhook-shape decision envelope
// (decision ∈ {APPROVE, REJECT_HARD, REJECT_SOFT, MODIFY, ABSTAIN}).
//
// OpenAPI: docs/users/api/openapi/admin/e31-s2-aiguard-compliance-webhook-integration.yaml
// Handler: packages/ai-gateway/internal/ingress/proxy/classify/classify.go
//          (ServeComplianceWebhookHTTP)
// Wiring : packages/ai-gateway/cmd/ai-gateway/wiring/thingclient.go
//          (MountAIGuardRoutes — mounted WITHOUT rstokenauth middleware
//          by design; this is an internal webhook surface protected by
//          network ACLs, not VK auth. The OpenAPI spec carries no
//          `security` block, confirming the contract.)
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
//   1. Happy path — well-formed ComplianceWebhookRequest (per the
//      OpenAPI schema): stage/method/path/targetHost/model/ingressType +
//      normalizedContent. The handler always returns a structured JSON
//      decision envelope; HTTP status is 200 when the classifier ran
//      (regardless of which decision it produced) or 503 when the
//      AIGuard backend is unavailable (acceptable in CI where the
//      backend is often unconfigured). Both responses MUST carry a JSON
//      body with a known shape — status==200 → ComplianceWebhookResponse
//      with `decision` in the documented enum; status==503 → ErrorBody
//      with `error`.
//
//   2. Malformed body — POST `{` (invalid JSON). Handler responds
//      HTTP 400 with ErrorBody{error:"malformed_json"}. This proves the
//      decode-error branch is wired and prevents a regression where a
//      panic-on-decode would crash the gateway under a malformed
//      webhook payload from a misbehaving compliance-proxy build.
//
//   3. No-auth admittance — POST with NO Authorization header. The
//      endpoint is intentionally mounted without the rstokenauth
//      middleware that fronts /v1/ai-guard/classify; the OpenAPI spec
//      carries no `security` block. The expected behavior is "the
//      endpoint accepts anonymous POSTs and runs the classifier" —
//      identical to Arm 1's behaviour. This arm DOCUMENTS that
//      property so a regression that accidentally adds auth (causing
//      every webhook-forward call from the compliance-proxy to start
//      401-ing) is caught immediately. If a future build intentionally
//      adds auth, this arm is the canary that forces the OpenAPI spec
//      + this scenario + the compliance-proxy webhook plumbing to be
//      updated in lockstep.
//
// Metric: nexus_ai_gateway_requests_total — per the same labelling
// convention used by S-062, this counter ticks on every ingress request
// regardless of the eventual HTTP status. We assert delta ≥ 3 across
// the three arms via CounterSum (label-free, sums all label
// permutations) so the test is robust whether or not the counter
// carries a `path` label in this build.
func TestS086_AIGuardComplianceWebhook(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	// Mint a fresh VK for arms 1 + 2 even though the endpoint is
	// unauth'd in this build — exercising it with a real Bearer header
	// also confirms the handler doesn't choke on an unexpected
	// Authorization header (forward-compat: if auth is added later,
	// arms 1+2 will still pass). Arm 3 omits the header to assert the
	// current no-auth contract.
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
	statusA, bodyA, err := intg.AIGwPostJSON(&envForCall, client,
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
	statusB, bodyB, err := intg.AIGwPostJSON(&envForCall, client,
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
	switch statusC {
	case 200, 503:
		// Documented no-auth contract: anonymous POSTs are accepted
		// and either evaluated (200) or fall through to backend-
		// unavailable (503). Both prove no auth gate is in front of
		// the handler — which is the intended state.
		t.Logf("Arm C OK: no-auth contract upheld (status=%d, body=%q)",
			statusC, truncate(bodyC, 200))
	case 401, 403:
		// A regression that adds auth to this route. The compliance-
		// proxy webhook-forward integration would silently break in
		// prod — fail loudly here so the lockstep update (OpenAPI
		// `security` block + compliance-proxy auth wiring + this
		// scenario) is forced into the same PR.
		t.Errorf("Arm C: endpoint unexpectedly rejected anonymous POST with status=%d — OpenAPI spec documents no `security` block; if auth was added intentionally, update the yaml + compliance-proxy + this scenario in lockstep (body=%q)",
			statusC, truncate(bodyC, 200))
	default:
		t.Errorf("Arm C: unexpected status=%d (want 200|503 for no-auth contract, or 401|403 for auth regression) (body=%q)",
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
	if decisionsDelta < 2 {
		t.Errorf("nexus_aiguard_decisions_total delta=%g (want ≥ 2 across arms A+C)", decisionsDelta)
	}
	if cacheLookupsDelta < 2 {
		t.Errorf("nexus_aiguard_cache_{hits,misses}_total delta=%g (want ≥ 2)", cacheLookupsDelta)
	}
	reqDelta := decisionsDelta // kept for the OK log line below

	t.Logf("S-086 OK: armA=%d armB=%d armC=%d req_delta=%.0f",
		statusA, statusB, statusC, reqDelta)
}
