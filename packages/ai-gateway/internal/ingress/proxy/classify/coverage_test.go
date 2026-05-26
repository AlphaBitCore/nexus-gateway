// coverage_test.go — white-box tests for unexported helpers in package classify.
// Moved here from handler/coverage_gaps_test.go and handler/coverage_boost_test.go
// when classify was extracted from the handler package.
package classify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
)

// errClassifier is a stub Classifier that returns a scripted error.
type errClassifier struct{ err error }

func (e *errClassifier) Classify(_ context.Context, _ aiguard.Request) (*aiguard.Response, error) {
	return nil, e.err
}

func TestIntField_AllVariants(t *testing.T) {
	p := map[string]any{
		"f":   float64(7),
		"i":   int(9),
		"s":   "12",
		"nil": nil,
	}
	if got := intField(p, "f"); got != 7 {
		t.Errorf("float64: got %d", got)
	}
	if got := intField(p, "i"); got != 9 {
		t.Errorf("int: got %d", got)
	}
	if got := intField(p, "s"); got != 0 {
		t.Errorf("string fallthrough: got %d want 0", got)
	}
	if got := intField(p, "missing"); got != 0 {
		t.Errorf("missing: got %d", got)
	}
	if got := intField(p, "nil"); got != 0 {
		t.Errorf("nil value: got %d", got)
	}
}

func TestStringSliceField_NonSliceFallthrough(t *testing.T) {
	p := map[string]any{
		"k": "not-a-slice",
	}
	if got := stringSliceField(p, "k"); got != nil {
		t.Errorf("non-slice value should return nil; got %v", got)
	}
}

func TestStringSliceField_NonStringElement(t *testing.T) {
	p := map[string]any{
		"k": []any{"ok", 42, "yes"},
	}
	got := stringSliceField(p, "k")
	if len(got) != 2 || got[0] != "ok" || got[1] != "yes" {
		t.Errorf("non-string element should be dropped; got %v", got)
	}
}

func TestFirstTextField_FirstMatchWins(t *testing.T) {
	p := map[string]any{"a": "", "b": "  ", "c": "yes"}
	if got := firstTextField(p, "a", "b", "c"); got != "yes" {
		t.Errorf("got %q", got)
	}
	if got := firstTextField(p, "a", "b"); got != "" {
		t.Errorf("all empty: got %q", got)
	}
}

func TestExtractMessagesText_OpenAIShape(t *testing.T) {
	p := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "hi"},
		},
	}
	got := extractMessagesText(p)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "hi") {
		t.Errorf("expected joined messages; got %q", got)
	}
}

func TestExtractMessagesText_WrappedInputShape(t *testing.T) {
	p := map[string]any{
		"input": map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "ping"},
			},
		},
	}
	if got := extractMessagesText(p); !strings.Contains(got, "ping") {
		t.Errorf("expected ping in extract; got %q", got)
	}
}

func TestExtractMessagesText_Missing(t *testing.T) {
	if got := extractMessagesText(map[string]any{}); got != "" {
		t.Errorf("empty payload should return empty; got %q", got)
	}
}

func TestExtractMessagesFromValue_NonSlice(t *testing.T) {
	if got := extractMessagesFromValue("not a slice"); got != "" {
		t.Errorf("non-slice should return empty; got %q", got)
	}
}

func TestExtractMessagesFromValue_ContentVariants(t *testing.T) {
	rows := []any{
		map[string]any{"role": "user", "content": "plain string"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "block-1"},
			map[string]any{"type": "image_url"},
			map[string]any{"type": "text", "text": "block-2"},
		}},
		map[string]any{"role": "assistant"},
	}
	got := extractMessagesFromValue(rows)
	for _, want := range []string{"plain string", "block-1", "block-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestExtractMessagesFromValue_RowNotMap(t *testing.T) {
	got := extractMessagesFromValue([]any{
		"not-a-map",
		map[string]any{"content": "ok"},
	})
	if got != "ok" {
		t.Errorf("got=%q want ok (non-map row skipped)", got)
	}
}

func TestExtractMessagesFromValue_BlockNotMap(t *testing.T) {
	got := extractMessagesFromValue([]any{
		map[string]any{
			"content": []any{
				"not-a-map",
				map[string]any{"text": "ok"},
			},
		},
	})
	if got != "ok" {
		t.Errorf("got=%q want ok (non-map block skipped)", got)
	}
}

func TestToWebhookDecision_AllCases(t *testing.T) {
	cases := map[string]string{
		"reject_hard":     "REJECT_HARD",
		"REJECT_HARD":     "REJECT_HARD",
		"reject_soft":     "BLOCK_SOFT",
		"modify":          "MODIFY",
		"abstain":         "ABSTAIN",
		"approve":         "APPROVE",
		"":                "APPROVE",
		"  reject_hard  ": "REJECT_HARD",
		"unknown":         "APPROVE",
	}
	for in, want := range cases {
		if got := toWebhookDecision(in); got != want {
			t.Errorf("toWebhookDecision(%q)=%q want %q", in, got, want)
		}
	}
}

// firstNonEmpty / joinNonEmpty

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := firstNonEmpty([]string{"", "  ", "first", "second"}); got != "first" {
		t.Errorf("got %q want first", got)
	}
	if got := firstNonEmpty([]string{"", "  "}); got != "" {
		t.Errorf("all empty: got %q", got)
	}
}

func TestJoinNonEmpty(t *testing.T) {
	if got := joinNonEmpty(nil, ","); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := joinNonEmpty([]string{"a", " ", "b", "", "c"}, ","); got != "a,b,c" {
		t.Errorf("got %q want a,b,c", got)
	}
}

func TestWebhookPayloadContent_PrefersNormalizedContent(t *testing.T) {
	p := map[string]any{
		"normalizedContent": []any{"first", "second"},
		"input":             "ignored",
	}
	req := payloadToComplianceWebhookRequest(p)
	got, src := webhookPayloadContent(p, req)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("normalizedContent should win; got %q", got)
	}
	if src != contentSourceNormalized {
		t.Errorf("source=%v want normalized", src)
	}
}

func TestWebhookPayloadContent_FallsBackToMessages(t *testing.T) {
	p := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "msg-text"},
		},
	}
	req := payloadToComplianceWebhookRequest(p)
	got, src := webhookPayloadContent(p, req)
	if !strings.Contains(got, "msg-text") {
		t.Errorf("expected msg-text; got %q", got)
	}
	if src != contentSourceMessages {
		t.Errorf("source=%v want messages", src)
	}
}

func TestWebhookPayloadContent_FallsBackToTextField(t *testing.T) {
	p := map[string]any{
		"prompt": "the prompt",
	}
	req := payloadToComplianceWebhookRequest(p)
	got, src := webhookPayloadContent(p, req)
	if got != "the prompt" {
		t.Errorf("got %q", got)
	}
	if src != contentSourceTextField {
		t.Errorf("source=%v want textField", src)
	}
}

func TestWebhookPayloadContent_FallsBackToStageJoin(t *testing.T) {
	p := map[string]any{
		"stage":      "request",
		"method":     "POST",
		"path":       "/v1/x",
		"targetHost": "api.example.com",
		"model":      "gpt-4o",
	}
	req := payloadToComplianceWebhookRequest(p)
	got, src := webhookPayloadContent(p, req)
	if !strings.Contains(got, "request") || !strings.Contains(got, "POST") {
		t.Errorf("stage join missing; got %q", got)
	}
	if src != contentSourceTextField {
		t.Errorf("source=%v want textField", src)
	}
}

func TestWebhookPayloadContent_FallsBackToRawPayload(t *testing.T) {
	p := map[string]any{"meta": "only"}
	req := payloadToComplianceWebhookRequest(p)
	got, src := webhookPayloadContent(p, req)
	if got == "" {
		t.Error("expected non-empty fallback")
	}
	if src != contentSourcePayloadRaw {
		t.Errorf("source=%v want payloadRaw", src)
	}
}

func TestWebhookPayloadContent_DefaultEmpty(t *testing.T) {
	p := map[string]any{}
	req := payloadToComplianceWebhookRequest(p)
	got, src := webhookPayloadContent(p, req)
	if got != "compliance-webhook request" {
		t.Errorf("default fallback: got %q", got)
	}
	if src != contentSourcePayloadRaw {
		t.Errorf("source=%v want payloadRaw", src)
	}
}

func TestWebhookReasonCode_LabelsWin(t *testing.T) {
	resp := &aiguard.Response{Labels: []string{"pii.email", "pii.ssn"}}
	got := webhookReasonCode(resp, contentSourceNormalized)
	if got != "pii.email" {
		t.Errorf("got %q want pii.email (first label)", got)
	}
}

func TestWebhookReasonCode_EmptyLabelsFallsBackToSource(t *testing.T) {
	resp := &aiguard.Response{}
	got := webhookReasonCode(resp, contentSourceMessages)
	if got != string(contentSourceMessages) {
		t.Errorf("got %q", got)
	}
}

func TestWebhookReasonCode_NilResp(t *testing.T) {
	got := webhookReasonCode(nil, contentSourcePayloadRaw)
	if got != string(contentSourcePayloadRaw) {
		t.Errorf("got %q", got)
	}
}

// ServeClassifyHTTP / ServeComplianceWebhookHTTP

func TestServeClassifyHTTP_MalformedJSON(t *testing.T) {
	h := NewClassifyHandler(&errClassifier{})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/classify",
		strings.NewReader(`{bad json`))
	w := httptest.NewRecorder()
	h.ServeClassifyHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "malformed_json") {
		t.Errorf("body=%s want malformed_json", w.Body.String())
	}
}

func TestServeClassifyHTTP_MissingField(t *testing.T) {
	h := NewClassifyHandler(&errClassifier{})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/classify",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeClassifyHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing_required_field") {
		t.Errorf("body=%s want missing_required_field", w.Body.String())
	}
}

func TestServeClassifyHTTP_InternalError(t *testing.T) {
	h := NewClassifyHandler(&errClassifier{err: errors.New("synth fail")})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/classify",
		strings.NewReader(`{"detector_type":"x","content":"y"}`))
	w := httptest.NewRecorder()
	h.ServeClassifyHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500; body=%s", w.Code, w.Body.String())
	}
}

func TestServeComplianceWebhookHTTP_InternalError(t *testing.T) {
	h := NewClassifyHandler(&errClassifier{err: errors.New("synth fail")})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/compliance-webhook",
		strings.NewReader(`{"stage":"request","method":"POST","path":"/x","model":"m","ingressType":"AI_GATEWAY","normalizedContent":["x"]}`))
	w := httptest.NewRecorder()
	h.ServeComplianceWebhookHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500; body=%s", w.Code, w.Body.String())
	}
}
