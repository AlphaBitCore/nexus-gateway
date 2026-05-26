// Webhook-forward hook scenario (S-069) — verifies that the
// `webhook-forward` hook implementation actually performs an outbound
// HTTP POST to its configured endpoint and that the parsed decision
// drives the inflight outcome (approve → 200, reject → 4xx).
//
// Lives at the AI Gateway request stage (the only data-plane service
// with outbound HTTP egress wired for hook plugins per
// packages/shared/policy/hooks/webhook/webhook.go's audit #15 caveat).
// Cross-service: AI Gw hook eval → external httptest server → AI Gw
// proxy path → DB traffic_event.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS069_WebhookForwardHook — PM-grade e2e.
//
// BRAINSTORM (pre): the webhook-forward hook is the only built-in
// dispatcher that hands compliance decisions to an out-of-process
// judge. The end-to-end invariants are:
//
//	(1) AI Gw actually POSTs to the configured endpoint when the hook
//	    fires on /v1/chat/completions (proved by an in-test hit counter
//	    incremented inside the httptest.Handler).
//	(2) The hook honours the webhook's `decision` field — approve →
//	    request goes through (HTTP 200, normal upstream traffic_event),
//	    reject → request is short-circuited (non-2xx, terminal hook
//	    decision recorded in traffic_event).
//	(3) Two requests across the suite move
//	    `hook_pipeline_total{stage="request"}` by ≥ 2 (one APPROVE
//	    on the approve arm + one REJECT_HARD on the deny arm). This
//	    is the real Prometheus name emitted by the AI Gateway —
//	    confirmed against `curl :3050/metrics` and the opsmetrics
//	    registration site at
//	    packages/ai-gateway/internal/platform/metrics/metrics.go:100
//	    (dotted name `hook.pipeline_total` → snake-cased on export).
//
// Contract notes (verified against admin handler + webhook.go):
//   - admin POST /api/admin/hooks expects `type ∈ {builtin, webhook,
//     script}` (NOT the implementation id) + a separate
//     `implementationId`. The hook impl is "webhook-forward"; the
//     enum-shaped type is "webhook".
//   - The remote URL travels via the top-level `endpoint` column —
//     core.BuildHookConfig folds it into cfg.Config["endpoint"] at
//     resolver build time, so the webhook factory reads it uniformly.
//   - webhook.go's decision mapping (line 213-222) treats
//     "reject"/"reject_hard" as core.RejectHard. The user-facing
//     "deny" string is NOT in the switch and would fall through to
//     Approve — we use "reject" so the deny arm actually denies.
func TestS069_WebhookForwardHook(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// ----- Arm 1: external webhook server with deterministic JSON. -----
	//
	// `hits` proves the webhook hook actually reached out; `denyMode`
	// flips the response between the approve and reject arms without
	// tearing the server down. atomic.Int64 keeps both safe across the
	// AI Gw goroutine that calls the webhook and the test goroutine
	// that flips the flag.
	var hits atomic.Int64
	var denyMode atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if denyMode.Load() {
			// `reject` maps to core.RejectHard in webhook.go's decision
			// switch — gives the AI Gw a terminal block decision.
			_, _ = w.Write([]byte(`{"decision":"reject","reason":"test-deny","reasonCode":"S069_DENY"}`))
			return
		}
		_, _ = w.Write([]byte(`{"decision":"approve","reason":"test-approve","reasonCode":"S069_OK"}`))
	}))
	sc.Cleanup.Register("httptest.Server.Close", func() error {
		srv.Close()
		return nil
	})

	// ----- Arm 2: create the hook via admin API. -----
	//
	// `type` is the admin enum (`webhook`), `implementationId` is the
	// concrete factory key. Endpoint goes to the top-level column so
	// BuildHookConfig folds it into cfg.Config["endpoint"]. `enabled`
	// defaults true; we set it explicitly for clarity.
	createBody := mustMarshal(t, map[string]any{
		"name":             fmt.Sprintf("s069-webhook-%d", time.Now().UnixNano()),
		"type":             "webhook",
		"implementationId": "webhook-forward",
		"stage":            "request",
		"endpoint":         srv.URL,
		"priority":         10,
		"timeoutMs":        2000,
		"failBehavior":     "fail-open",
		"enabled":          true,
		"config": map[string]any{
			"payloadMode": "metadata-only",
			"onMatch": map[string]any{
				// Inflight block-hard so the webhook's `decision:reject`
				// translates into a real 4xx rather than getting downgraded
				// to an audit-only annotation. The onMatch enum vocabulary
				// (parseInflightAction in packages/shared/policy/hooks/core/
				// onmatch.go) is {approve, block-hard, block-soft, redact};
				// "reject_hard" is the webhook RESPONSE shape and is NOT
				// a valid onMatch value — feeding it here would fail at
				// hook construction with "unknown inflightAction".
				"inflightAction": "block-hard",
			},
		},
	})
	createStatus, createBytes, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/hooks", createBody)
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}
	if createStatus == http.StatusNotFound {
		t.Fatalf("S-069 requires webhook-forward hook implementation; got %d (%q) — admin route missing means /api/admin/hooks regressed",
			createStatus, truncate(createBytes, 200))
	}
	if createStatus >= 400 {
		body := strings.ToLower(string(createBytes))
		if strings.Contains(body, "not registered") ||
			strings.Contains(body, "type must be") ||
			strings.Contains(body, "not supported") {
			t.Fatalf("S-069 requires webhook-forward hook implementation; got %d (%q) — builtin factory `webhook-forward` is not wired in the gateway",
				createStatus, truncate(createBytes, 200))
		}
		t.Fatalf("create hook: status=%d body=%q", createStatus, truncate(createBytes, 300))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createBytes, &created); err != nil || created.ID == "" {
		t.Fatalf("create hook response missing id: err=%v body=%q",
			err, truncate(createBytes, 300))
	}
	sc.Cleanup.Register("DeleteHook("+created.ID+")", func() error {
		st, body, derr := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/admin/hooks/"+created.ID, nil)
		if derr != nil {
			return derr
		}
		if st >= 300 && st != http.StatusNotFound {
			return fmt.Errorf("delete hook %s: status=%d body=%q", created.ID, st, truncate(body, 200))
		}
		return nil
	})

	// Brief settle window: the create handler fires Hub invalidation
	// asynchronously; give the resolver a moment to pick up the new row
	// before the first chat call.
	time.Sleep(2 * time.Second)

	preMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics pre: %v", err)
	}

	// Per-arm VK keeps traffic_event predicates unambiguous and avoids
	// any prompt-cache cross-talk with sibling scenarios.
	vkName := fmt.Sprintf("s069-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()

	// ----- Arm 3a: approve path. -----
	hitsBeforeApprove := hits.Load()
	approveBody := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": fmt.Sprintf("Reply with exactly: WEBHOOK_OK nonce=%d", time.Now().UnixNano())},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	approveStatus, approveRespBody, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", approveBody)
	if err != nil {
		t.Fatalf("approve arm AIGwPostJSON: %v", err)
	}
	if approveStatus != 200 {
		// Tolerate a degraded gateway (rate limit, upstream 5xx) but
		// still require the webhook to have been called — otherwise
		// the hook never ran and this arm proves nothing.
		t.Logf("approve arm non-200 (status=%d body=%q) — checking webhook hit anyway",
			approveStatus, truncate(approveRespBody, 200))
	}
	if hits.Load() <= hitsBeforeApprove {
		// httptest.NewServer binds 127.0.0.1:<dyn> in this same process;
		// the local dev AI Gateway runs on the same host and dials it
		// directly (no docker network indirection). A miss here is a real
		// bug — either the hook config did not load (e.g. endpoint column
		// not selected by the AIGw loader, see hooks.go) or the pipeline
		// short-circuited before evaluating the hook. Fail loudly instead
		// of skipping so the regression surfaces.
		t.Fatalf("webhook hit counter did not advance on approve arm (before=%d after=%d, approveStatus=%d body=%q) — hook did not fire",
			hitsBeforeApprove, hits.Load(), approveStatus, truncate(approveRespBody, 200))
	}

	// ----- Arm 3b: deny path. Flip the server, fire again. -----
	denyMode.Store(true)
	hitsBeforeDeny := hits.Load()
	denyBody := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": fmt.Sprintf("Reply with exactly: WEBHOOK_DENY nonce=%d", time.Now().UnixNano())},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	denyStatus, denyRespBody, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", denyBody)
	if err != nil {
		t.Fatalf("deny arm AIGwPostJSON: %v", err)
	}
	if denyStatus == 200 {
		t.Errorf("deny arm expected non-2xx (webhook rejected), got 200 (body=%q)",
			truncate(denyRespBody, 300))
	}
	if hits.Load() <= hitsBeforeDeny {
		t.Fatalf("webhook hit counter did not advance on deny arm (before=%d after=%d) — hook did not fire",
			hitsBeforeDeny, hits.Load())
	}

	// ----- Cross-checks. -----
	totalHits := hits.Load()
	if totalHits < 2 {
		t.Errorf("webhook hit counter=%d (want ≥ 2 across approve+deny arms)", totalHits)
	}

	postMetrics, err := helpers.ScrapeMetrics(ctx, sc.Env.AIGwURL)
	if err != nil {
		t.Fatalf("ScrapeMetrics post: %v", err)
	}
	// Real metric: every webhook hook fires at request stage, so both
	// the approve arm (decision=APPROVE) and the deny arm (decision=
	// REJECT_HARD) increment hook_pipeline_total{stage="request"}.
	// Sum across decisions catches both arms in one delta. Hard-assert
	// — a miss here would mean the hook never ran, in which case the
	// httptest hit counter check above should have already failed; this
	// is the cross-check from the Prometheus side.
	hookLabels := map[string]string{"stage": "request"}
	reqDelta := postMetrics.CounterSum("hook_pipeline_total", hookLabels) -
		preMetrics.CounterSum("hook_pipeline_total", hookLabels)
	if reqDelta < 2 {
		t.Errorf("hook_pipeline_total{stage=\"request\"} delta=%g, want ≥ 2 (one per arm — approve + deny)", reqDelta)
	}

	// Traffic event for the deny arm: the AI Gw must have stamped a
	// terminal hook decision. We accept any non-APPROVE decision in
	// the {REJECT_HARD, REJECT_SOFT, REDACT} set — webhook.go produces
	// RejectHard for "reject"/"reject_hard", but the onMatch ceiling
	// can downgrade based on configured inflightAction.
	predicate := fmt.Sprintf(`source = 'ai-gateway'
		 AND identity->'vk'->>'id' = '%s'
		 AND request_hook_decision IN ('REJECT_HARD','REJECT_SOFT','REDACT')`, vk.ID)
	row, err := intg.WaitForRecentAuditEvent(
		context.Background(), sc.DB, predicate, nil, 45*time.Second,
	)
	if err != nil {
		t.Fatalf("traffic_event poll (deny arm): %v", err)
	}
	if row == nil {
		t.Errorf("no terminal-decision traffic_event row for VK %s — webhook reject did not stamp a hook decision", vk.ID)
	}

	t.Logf("S-069 OK: hook=%s hits=%d req_delta=%.0f approve_status=%d deny_status=%d audit=%v",
		created.ID, totalHits, reqDelta, approveStatus, denyStatus,
		func() string {
			if row == nil {
				return "<none>"
			}
			return row.ID + " (" + row.RequestHookDecision + ")"
		}())
}
