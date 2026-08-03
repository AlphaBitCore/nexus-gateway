package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	gr "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/guardrail"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func doGuardrail(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/guardrail", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func decodeGuardrail(t *testing.T, rr *httptest.ResponseRecorder) gr.Response {
	t.Helper()
	var resp gr.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode guardrail response: %v; body=%s", err, rr.Body.String())
	}
	return resp
}

// TestServeGuardrail_RedactVerdict_EndToEnd drives the real PII pipeline through
// the handler: a prompt with an email yields action=redact, the flat span, and
// the sanitized text — proving BuildPipeline→Execute→MapResult wiring and the
// same-policy-as-inline parity (the same pii-detector hook the inline path uses).
func TestServeGuardrail_RedactVerdict_EndToEnd(t *testing.T) {
	deps, prod := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	aw := deps.AuditWriter
	h := NewHandler(deps).ServeGuardrail()

	rr := doGuardrail(h, `{"stage":"input","content":"ping alice@example.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGuardrail(t, rr)
	if resp.Action != "redact" {
		t.Fatalf("action = %q, want redact; resp=%+v", resp.Action, resp)
	}
	if resp.Coverage != gr.CoverageFull {
		t.Errorf("coverage = %q, want full", resp.Coverage)
	}
	if len(resp.Redactions) != 1 || resp.Redactions[0].Replacement != "[REDACTED_EMAIL]" {
		t.Fatalf("redactions = %+v, want one [REDACTED_EMAIL]", resp.Redactions)
	}
	// email "alice@example.com" starts at byte 5 of "ping alice@example.com".
	if resp.Redactions[0].Start != 5 || resp.Redactions[0].End != 22 {
		t.Errorf("redaction offsets = [%d,%d), want [5,22)", resp.Redactions[0].Start, resp.Redactions[0].End)
	}
	if resp.RedactedContent != "ping [REDACTED_EMAIL]" {
		t.Errorf("redacted_content = %q, want %q", resp.RedactedContent, "ping [REDACTED_EMAIL]")
	}

	// One audit row was emitted for the verdict.
	aw.Close()
	prod.mu.Lock()
	n := len(prod.messages)
	prod.mu.Unlock()
	if n != 1 {
		t.Fatalf("captured %d audit messages, want 1", n)
	}
}

func TestServeGuardrail_BenignAllow(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps).ServeGuardrail()

	rr := doGuardrail(h, `{"content":"hello world"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := decodeGuardrail(t, rr)
	if resp.Action != "allow" || resp.Coverage != gr.CoverageFull {
		t.Fatalf("benign verdict = action=%q coverage=%q, want allow/full", resp.Action, resp.Coverage)
	}
	if len(resp.Redactions) != 0 || len(resp.Assessments) != 0 || resp.RedactedContent != "" {
		t.Fatalf("benign must carry no redactions/assessments, got %+v", resp)
	}
}

func TestServeGuardrail_NoHooks_AllowNoneCoverage(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = hookCacheWith(t) // empty → nil pipeline
	h := NewHandler(deps).ServeGuardrail()

	rr := doGuardrail(h, `{"content":"anything with alice@example.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := decodeGuardrail(t, rr)
	if resp.Action != "allow" || resp.Coverage != gr.CoverageNone {
		t.Fatalf("no-hooks verdict = action=%q coverage=%q, want allow/none", resp.Action, resp.Coverage)
	}
}

func TestServeGuardrail_OutputStage_NoRequestHooks(t *testing.T) {
	// The PII hook is stage=request; stage=output resolves no hooks → none.
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps).ServeGuardrail()

	rr := doGuardrail(h, `{"stage":"output","content":"ping alice@example.com"}`)
	resp := decodeGuardrail(t, rr)
	if resp.Coverage != gr.CoverageNone || resp.Action != "allow" {
		t.Fatalf("output-stage (no response hooks) = action=%q coverage=%q, want allow/none", resp.Action, resp.Coverage)
	}
}

func TestServeGuardrail_DefaultStageIsInput(t *testing.T) {
	// Omitting stage must default to input → the request-stage PII hook fires.
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps).ServeGuardrail()

	rr := doGuardrail(h, `{"content":"ping alice@example.com"}`)
	resp := decodeGuardrail(t, rr)
	if resp.Action != "redact" {
		t.Fatalf("omitted stage did not default to input (no redact): %+v", resp)
	}
}

func TestServeGuardrail_MessagesInput(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps).ServeGuardrail()

	rr := doGuardrail(h, `{"messages":[{"role":"user","content":"mail me at bob@corp.io"}]}`)
	resp := decodeGuardrail(t, rr)
	if resp.Action != "redact" || resp.RedactedContent != "mail me at [REDACTED_EMAIL]" {
		t.Fatalf("messages redact = %+v", resp)
	}
}

func TestServeGuardrail_AuthReject(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = hookCacheWith(t)
	deps.VKAuth = stubAuthErrSTT{}
	h := NewHandler(deps).ServeGuardrail()
	rr := doGuardrail(h, `{"content":"hi"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestServeGuardrail_RateLimited(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = hookCacheWith(t)
	rpm := 5
	deps.VKAuth = rpmVKAuth{rpm: &rpm}
	deps.RateLimiter = denyAllRateLimiter{}
	h := NewHandler(deps).ServeGuardrail()
	rr := doGuardrail(h, `{"content":"hi"}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set on rate-limit rejection")
	}
}

func TestServeGuardrail_GenCapReject(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = hookCacheWith(t)
	handler := NewHandler(deps)
	caps, ok := generativecaps.Lookup(typology.EndpointKindGuardrail)
	if !ok || caps.MaxConcurrentPerVK <= 0 {
		t.Fatalf("guardrail caps not configured: %+v ok=%v", caps, ok)
	}
	for i := range caps.MaxConcurrentPerVK {
		if !handler.genConcurrency.Acquire(typology.EndpointKindGuardrail, "vk-1", caps.MaxConcurrentPerVK) {
			t.Fatalf("pre-acquire %d failed", i)
		}
	}
	h := handler.ServeGuardrail()
	rr := doGuardrail(h, `{"content":"hi"}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (gen-cap saturated)", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set on gen-cap rejection")
	}
	if !strings.Contains(rr.Body.String(), "GENERATIVE_CONCURRENCY_LIMIT") {
		t.Errorf("expected GENERATIVE_CONCURRENCY_LIMIT, got %s", rr.Body.String())
	}
}

func TestServeGuardrail_BadRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"content":`},
		{"unknown field", `{"content":"x","bogus":1}`},
		{"neither content nor messages", `{}`},
		{"both content and messages", `{"content":"a","messages":[{"role":"user","content":"b"}]}`},
		{"whitespace content", `{"content":"   "}`},
		{"bad stage", `{"stage":"sideways","content":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps, _ := sttDeps(t, "http://127.0.0.1:0")
			deps.HookConfigCache = hookCacheWith(t)
			h := NewHandler(deps).ServeGuardrail()
			rr := doGuardrail(h, c.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body=%s", c.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestServeGuardrail_BodyTooLarge(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = hookCacheWith(t)
	h := NewHandler(deps).ServeGuardrail()
	// Content over the 1 MiB guardrail cap → 413.
	big := strings.Repeat("a", (1<<20)+1024)
	rr := doGuardrail(h, `{"content":"`+big+`"}`)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (over body cap)", rr.Code)
	}
}

func TestStampGuardrailAudit(t *testing.T) {
	// nil result → clean approve, StatusCode 200, NO raw body persisted.
	rec := &audit.Record{}
	stampGuardrailAudit(rec, nil, gr.CoverageNone, "request")
	if rec.StatusCode != http.StatusOK {
		t.Errorf("nil-result StatusCode = %d, want 200", rec.StatusCode)
	}
	if rec.ComplianceCoverage != gr.CoverageNone {
		t.Errorf("coverage = %q, want none", rec.ComplianceCoverage)
	}
	if rec.HookDecision != string(hookcore.Approve) {
		t.Errorf("nil-result HookDecision = %q, want APPROVE", rec.HookDecision)
	}
	if len(rec.RequestBody) != 0 {
		t.Errorf("guardrail must persist NO raw request body, got %d bytes", len(rec.RequestBody))
	}
	// D-N8: guardrail captures neither direction — the normalized view stays
	// 404 by design. Lock the response body empty too (the parallel-capture
	// path must never reach guardrail).
	if len(rec.ResponseBody) != 0 {
		t.Errorf("guardrail must persist NO raw response body, got %d bytes", len(rec.ResponseBody))
	}
	if rec.ResponseAction != "" {
		t.Errorf("guardrail must not stamp ResponseAction, got %q", rec.ResponseAction)
	}

	// A block result on the OUTPUT stage → decision/coverage/action stamped,
	// still no raw body, and the hook trace labeled "response" (not "request").
	rec2 := &audit.Record{}
	result := &hookcore.CompliancePipelineResult{
		Decision:    hookcore.RejectHard,
		Reason:      "blocked",
		ReasonCode:  "KEYWORD_BLOCKED",
		Tags:        []string{"keyword:x"},
		HookResults: []hookcore.HookResult{{HookName: "keyword", Decision: hookcore.RejectHard}},
	}
	stampGuardrailAudit(rec2, result, gr.CoverageFull, "response")
	if rec2.HookDecision != string(hookcore.RejectHard) || rec2.ComplianceCoverage != gr.CoverageFull {
		t.Errorf("block stamp wrong: decision=%q coverage=%q", rec2.HookDecision, rec2.ComplianceCoverage)
	}
	if string(rec2.RequestAction) != "block" {
		t.Errorf("RequestAction = %q, want block", rec2.RequestAction)
	}
	if len(rec2.RequestBody) != 0 {
		t.Errorf("block path must persist NO raw request body, got %d bytes", len(rec2.RequestBody))
	}
	if len(rec2.HooksPipeline) != 1 || rec2.HooksPipeline[0].Stage != "response" {
		t.Errorf("output-stage hook trace stage = %+v, want one entry stage=response", rec2.HooksPipeline)
	}
}
