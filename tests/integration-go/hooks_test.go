// Phase 2 — hook pipeline integration tests.
//
// These exercise the AI Gateway over real HTTP and verify both immediate
// behaviour (HTTP status + body shape) and downstream effects
// (traffic_event row in Postgres) for the three terminal hook decisions:
// REJECT_HARD on PII, APPROVE on clean traffic, and the negative-auth path.
//
// They complement the bash smoke (which proves "is this endpoint reachable?")
// and the Phase 4 AI-judge tests (which evaluate "was the decision
// sensible?"). This layer pins the deterministic contract — the wire is
// exactly this, the audit row is exactly that — so a regression that
// downgrades 403 to 200 fails here, fast and cheaply.
//
// Skipped automatically (not failed) if NEXUS_TEST_VK is unset; running
// the suite without a VK should not produce noise.
package integrationgo_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// TestHooks_PIIRejectsSSN exercises E5 (PII Detector) end-to-end:
// the prompt contains an SSN, the gateway is expected to terminate the
// request with HTTP 403 + a body that names the detected PII type, and
// the corresponding traffic_event row carries status_code=403 and
// request_hook_decision='REJECT_HARD'. Uses the Wikipedia "always-invalid"
// SSN sentinel 123-45-6789 so a logging regression cannot exfiltrate
// real data.
func TestHooks_PIIRejectsSSN(t *testing.T) {
	env, vk, db := setupOrSkip(t)

	body := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": "Customer record: SSN 123-45-6789. One-line summary."},
		},
		"max_tokens":  16,
		"temperature": 0,
	})
	_ = vk // reserved for assertions added below

	client := helpers.LocalHTTPClient()
	status, respBody, err := helpers.AIGwPostJSON(env, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	if status != 403 {
		t.Fatalf("expected HTTP 403, got %d (body=%q)", status, truncate(respBody, 200))
	}
	lower := strings.ToLower(string(respBody))
	if !strings.Contains(lower, "pii") && !strings.Contains(lower, "ssn") {
		t.Fatalf("rejection body must name PII or SSN, got %q", truncate(respBody, 200))
	}

	// Audit cross-check. Filter narrowly so a concurrent 200 from the
	// approve test does not race in.
	row, err := helpers.WaitForRecentAuditEvent(
		context.Background(), db,
		`source = 'ai-gateway'
		 AND path = '/v1/chat/completions'
		 AND status_code = 403
		 AND request_hook_decision = 'REJECT_HARD'`,
		nil,
		45*time.Second,
	)
	if err != nil {
		t.Fatalf("audit poll failed: %v", err)
	}
	if row == nil {
		t.Fatalf("no traffic_event row with status=403, REJECT_HARD appeared within 45s")
	}
	t.Logf("audit row id=%s decision=%s status=%d model=%s",
		row.ID, row.RequestHookDecision, row.StatusCode, row.ModelName)
}

// TestHooks_ApproveCleanPrompt is the contrast case: a prompt with no PII
// and no policy triggers gets HTTP 200 with a chat completion envelope,
// and the audit row carries status_code=200 and APPROVE. Without this
// counterpart the suite cannot tell "is the gateway dead" apart from
// "are hooks blocking everything".
func TestHooks_ApproveCleanPrompt(t *testing.T) {
	env, _, db := setupOrSkip(t)

	body := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly: APPROVE_OK"},
		},
		"max_tokens":  8,
		"temperature": 0,
	})

	client := helpers.LocalHTTPClient()
	status, respBody, err := helpers.AIGwPostJSON(env, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d (body=%q)", status, truncate(respBody, 200))
	}

	// Shape check: must be an OpenAI-style chat completion.
	var parsed struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody=%q", err, truncate(respBody, 200))
	}
	if parsed.Object != "chat.completion" {
		t.Errorf("response.object=%q, want chat.completion", parsed.Object)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		t.Errorf("response missing choices[0].message.content: %+v", parsed)
	}
	if parsed.Usage.TotalTokens == 0 {
		t.Errorf("response.usage.total_tokens not populated: %+v", parsed.Usage)
	}

	row, err := helpers.WaitForRecentAuditEvent(
		context.Background(), db,
		`source = 'ai-gateway'
		 AND path = '/v1/chat/completions'
		 AND status_code = 200
		 AND request_hook_decision = 'APPROVE'`,
		nil,
		45*time.Second,
	)
	if err != nil {
		t.Fatalf("audit poll failed: %v", err)
	}
	if row == nil {
		t.Fatalf("no traffic_event row with status=200, APPROVE appeared within 45s")
	}
	t.Logf("audit row id=%s decision=%s status=%d model=%s",
		row.ID, row.RequestHookDecision, row.StatusCode, row.ModelName)
}

// TestHooks_BadVKRejected verifies the negative-auth path: an obviously
// invalid VK on /v1/chat/completions must produce 401, not 200 (which
// would mean we silently fall through unauthenticated). /v1/models is
// optional-auth by design so the negative case is meaningful only on
// /v1/chat. Mirrors the same assertion the bash smoke makes — pinned in
// Go so a regression that flips the auth middleware off shows up in two
// suites independently.
func TestHooks_BadVKRejected(t *testing.T) {
	env, _, _ := setupOrSkip(t)

	body := mustMarshal(t, map[string]any{
		"model":      "moonshot-v1-8k",
		"messages":   []map[string]string{{"role": "user", "content": "x"}},
		"max_tokens": 1,
	})

	// Use a clearly-fake VK; the gateway's VK lookup must fail closed.
	client := helpers.LocalHTTPClient()
	envCopy := *env
	envCopy.TestVK = "nvk_definitely_not_a_real_key"
	status, _, err := helpers.AIGwPostJSON(&envCopy, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	if status != 401 && status != 403 {
		t.Fatalf("expected HTTP 401 or 403 for bogus VK, got %d", status)
	}
}
