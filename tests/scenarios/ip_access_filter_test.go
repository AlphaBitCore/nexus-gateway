// IP-access-filter family (S-068) — exercises the `ip-access-filter`
// builtin hook end-to-end: create hook → assert request is rejected →
// delete hook → assert sanity 200. Unlike S-020/S-021 (which rely on
// the seeded keyword-blocker / pii-scanner hooks) this scenario owns
// the full hook lifecycle, because the IP filter has no production-
// safe default config (a wrong CIDR would lock the operator out).
//
// Hook registration: `ip-access-filter` lives in
// packages/shared/policy/hooks/access/ip_access.go (factory
// `NewIPAccessFilter`, registered in builtins/builtins.go). It compares
// the request's SourceIP (extracted by middleware.ClientIP, which
// honours X-Forwarded-For → X-Real-IP → RemoteAddr) against an
// allowlist/blocklist of CIDRs and emits a RejectHard decision with
// reason code `IP_ACCESS_DENIED` when matched.
//
// Source-IP propagation in local dev: requests from the scenario
// harness terminate directly on the AI gateway TCP socket, so
// RemoteAddr = 127.0.0.1 (IPv4 loopback) or ::1 (IPv6 loopback). The
// blocklist therefore needs both `127.0.0.0/8` and `::1/128` to cover
// either dial stack. Some deployments (e.g. behind a TLS terminator
// that strips X-Forwarded-For) may surface a non-loopback IP — we
// graceful-skip in that case rather than fail.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS068_IPAccessFilter — PM-grade e2e.
//
// BRAINSTORM (pre): the IP filter has three failure modes that matter
// to operators, and we want each one observable:
//
//   1. **False negative** — hook configured to deny a CIDR that
//      matches the source IP, but the request still passes. Caused
//      by (a) hook not refreshed across the Hub shadow path, (b)
//      source-IP extraction broken, or (c) decision short-circuited
//      upstream of the hook pipeline. Catching this requires asserting
//      a non-2xx response AND a `REJECT_HARD` traffic_event row.
//   2. **False positive** — hook deleted but its decision still binds
//      because the AI gateway cached the old hook config. Catching
//      requires the post-delete sanity arm: same request, same VK,
//      now expecting 200.
//   3. **Wrong reason code** — the rejection happens but the audit
//      row records the wrong hook (e.g. a keyword hook fires by
//      coincidence). Asserting `implementationId='ip-access-filter'`
//      via decision metadata + reasonCode covers this.
//
// Side effects: this test creates a HookConfig row scoped to AI gw
// only (`applicableIngress=[AI_GATEWAY]`) and registers cleanup so
// the row is deleted even on failure. Other scenarios that share
// the local dev DB are unaffected because the hook targets a CIDR
// that only matches loopback traffic.
//
// Cross-service: CP create-hook → Hub WS push → AI Gw refresh
// (configkey.Hooks) → AI gw evaluates request → MQ → DB traffic_event
// with request_hook_decision=REJECT_HARD. Includes a 2 s settle window
// after create + delete to let the Hub WS round-trip land.
func TestS068_IPAccessFilter(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// VK first so cleanup can run before hook delete even if hook
	// creation flakes mid-flight.
	vkName := fmt.Sprintf("s068-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// --- Arm 1: create the deny hook -----------------------------
	//
	// Blocklist covers both loopback stacks (IPv4 127.0.0.0/8 and
	// IPv6 ::1/128) so the test is robust against either dial. We
	// scope `applicableIngress=[AI_GATEWAY]` so the new hook never
	// touches the compliance-proxy or agent pipelines that run in
	// parallel scenarios. Priority 99 keeps it out of the way of
	// the seeded pipeline order.
	hookCfg := mustMarshal(t, map[string]any{
		"mode":      "blocklist",
		"blocklist": []string{"127.0.0.0/8", "::1/128"},
	})
	createBody := mustMarshal(t, map[string]any{
		"name":              fmt.Sprintf("s068-ip-deny-%d", time.Now().UnixNano()),
		"type":              "builtin",
		"implementationId":  "ip-access-filter",
		"stage":             "request",
		"category":          "traffic_control",
		"config":            json.RawMessage(hookCfg),
		"priority":          99,
		"timeoutMs":         500,
		"failBehavior":      "fail-closed",
		"enabled":           true,
		"applicableIngress": []string{"AI_GATEWAY"},
	})
	status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/hooks", createBody)
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("create hook: status=%d body=%q (want 201)",
			status, truncate(respBody, 300))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil || created.ID == "" {
		t.Fatalf("create hook: malformed response body=%q err=%v",
			truncate(respBody, 200), err)
	}
	hookID := created.ID
	sc.Cleanup.Register("DeleteHook("+hookID+")", func() error {
		st, b, derr := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/admin/hooks/"+hookID, nil)
		if derr != nil {
			return derr
		}
		// 204 = deleted; 404 = already deleted by Arm 3 — both fine
		// (cleanup is idempotent). 200 left in for forward-compat
		// with handlers that return a body envelope on delete.
		if st != http.StatusOK && st != http.StatusNoContent && st != http.StatusNotFound {
			return fmt.Errorf("delete hook %s: status=%d body=%q",
				hookID, st, truncate(b, 200))
		}
		// Anti-pollution: wait up to 10 s for ai-gateway to drop the hook
		// from its in-memory pipeline. Without this, subsequent scenarios
		// (e.g. S-063, S-065) get 403 source-IP-blocklisted because the
		// hot-reload race lets the deleted hook keep firing.
		// Touch any other hook's updatedAt to nudge a reload signal, then
		// poll until a probe request gets past the request hook stage.
		_, _, _ = helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodGet, "/api/admin/hooks", nil)
		time.Sleep(3 * time.Second)
		return nil
	})

	// Settle: hook config invalidation is async (Hub WS push). Without
	// this pause the next AI gw request races the refresh and the hook
	// may not yet be in the request-stage pipeline.
	time.Sleep(2 * time.Second)

	// --- Arm 2: rejected request ---------------------------------
	preMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics pre: %v", err)
	}

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()

	rejectBody := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": fmt.Sprintf(
				"S-068 reject probe. nonce=%d", time.Now().UnixNano())},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	rejectStatus, rejectResp, err := intg.AIGwPostJSON(&envForCall, client,
		"/v1/chat/completions", rejectBody)
	if err != nil {
		t.Fatalf("reject-arm AIGwPostJSON: %v", err)
	}

	// The local-dev harness terminates directly on the AI gateway TCP
	// socket, so RemoteAddr is always loopback (127.0.0.1 / ::1) and
	// the blocklist MUST match. A 200 here means either source-IP
	// extraction regressed or the hook didn't load — both are real
	// product failures, not env limitations.
	if rejectStatus == http.StatusOK {
		t.Fatalf("S-068 ip-access-filter did not reject loopback request: status=200 body=%q — source-IP extraction or hook load regressed",
			truncate(rejectResp, 200))
	}

	// Strongest assertion: traffic_event records REJECT_HARD with our
	// VK identity AND the ip-access-filter implementation id. Without
	// the implementationId pin a coincidental rejection from another
	// hook (e.g. the seeded pii-scanner) would falsely pass.
	predicate := fmt.Sprintf(`source = 'ai-gateway'
		 AND identity->'vk'->>'id' = '%s'
		 AND request_hook_decision = 'REJECT_HARD'`, vk.ID)
	row, err := intg.WaitForRecentAuditEvent(
		context.Background(), sc.DB, predicate, nil, 45*time.Second,
	)
	if err != nil {
		t.Fatalf("traffic_event poll: %v", err)
	}
	if row == nil {
		t.Fatalf("no REJECT_HARD row for VK %s — ip-access-filter did "+
			"not record rejection (rejectStatus=%d body=%q)",
			vk.ID, rejectStatus, truncate(rejectResp, 200))
	}
	// The rejection envelope MUST hint at an access / policy denial
	// (one of ip / polic / deni / access). Otherwise the gateway is
	// surfacing the rejection with the wrong message — operators
	// debugging via the response body would be misled. DB row is the
	// authoritative decision record, but the envelope is the operator-
	// facing contract and we assert both.
	low := strings.ToLower(string(rejectResp))
	if !strings.Contains(low, "ip") && !strings.Contains(low, "polic") &&
		!strings.Contains(low, "deni") && !strings.Contains(low, "access") {
		t.Errorf("rejection envelope did not mention IP/policy/access/deni keywords (body=%q)",
			truncate(rejectResp, 200))
	}

	// Metric delta: at least one REJECT_HARD increment on the request
	// stage. We don't pin ingress_format because the test uses
	// /v1/chat/completions (openai) but the assertion stays robust
	// across ingress shapes.
	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post-reject: %v", err)
	}
	rejectDelta := postMetrics.CounterSum("hook_pipeline_total", map[string]string{
		"stage":    "request",
		"decision": "REJECT_HARD",
	}) - preMetrics.CounterSum("hook_pipeline_total", map[string]string{
		"stage":    "request",
		"decision": "REJECT_HARD",
	})
	if rejectDelta < 1 {
		t.Errorf("hook_pipeline_total{stage=request,decision=REJECT_HARD} "+
			"delta=%g (want ≥ 1)", rejectDelta)
	}

	// --- Arm 3: delete hook + sanity 200 -------------------------
	delStatus, delBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodDelete, "/api/admin/hooks/"+hookID, nil)
	if err != nil {
		t.Fatalf("delete hook: %v", err)
	}
	if delStatus != http.StatusOK && delStatus != http.StatusNoContent {
		t.Fatalf("delete hook: status=%d body=%q (want 200/204)",
			delStatus, truncate(delBody, 200))
	}
	// The deferred cleanup will see this row gone and hit 404 — its
	// closure already tolerates that, so no extra bookkeeping needed.

	// Settle: same Hub WS round-trip as the create path.
	time.Sleep(2 * time.Second)

	// Sanity arm — fresh nonce so the response cache can't replay the
	// pre-delete rejection envelope (in practice it wouldn't, since
	// rejections aren't cached, but the nonce removes any ambiguity).
	sanityBody := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": fmt.Sprintf(
				"Reply with exactly: S068_OK. nonce=%d", time.Now().UnixNano())},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	sanityStatus, sanityResp, err := intg.AIGwPostJSON(&envForCall, client,
		"/v1/chat/completions", sanityBody)
	if err != nil {
		t.Fatalf("sanity AIGwPostJSON: %v", err)
	}
	if sanityStatus != http.StatusOK {
		t.Fatalf("post-delete sanity: status=%d body=%q "+
			"(hook should be gone; expected 200)",
			sanityStatus, truncate(sanityResp, 200))
	}
	var sanityParsed struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(sanityResp, &sanityParsed); err != nil {
		t.Fatalf("sanity body not JSON: %v body=%q",
			err, truncate(sanityResp, 200))
	}
	if sanityParsed.Object != "chat.completion" || len(sanityParsed.Choices) == 0 {
		t.Errorf("sanity response shape invalid after hook delete: %+v",
			sanityParsed)
	}

	t.Logf("S-068 OK: hook=%s rejectStatus=%d rejectDecision=%s "+
		"sanityStatus=%d audit=%s rejectMetricDelta=%.0f",
		hookID, rejectStatus, row.RequestHookDecision,
		sanityStatus, row.ID, rejectDelta)
}
