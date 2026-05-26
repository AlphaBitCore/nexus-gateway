package pipeline

import (
	"strings"
	"testing"
)

func TestNullableString_EmptyReturnsNil(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("empty: got %v, want nil", got)
	}
}

func TestNullableString_NonEmptyReturnsFreshPointer(t *testing.T) {
	got := nullableString("approve")
	if got == nil {
		t.Fatal("non-empty: got nil")
	}
	if *got != "approve" {
		t.Errorf("got %q, want approve", *got)
	}
}

func TestSumHooksPipelineLatencyMs(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want *int
	}{
		{"nil yields nil", nil, nil},
		{"empty yields nil", []byte{}, nil},
		{"malformed JSON yields nil", []byte(`{not-json`), nil},
		{"empty array yields nil", []byte(`[]`), nil},
		{"sums positive latencies", []byte(`[{"latencyMs":10},{"latencyMs":15}]`), intPtr(25)},
		{"skips zero / negative", []byte(`[{"latencyMs":10},{"latencyMs":0},{"latencyMs":-5}]`), intPtr(10)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sumHooksPipelineLatencyMs(c.in)
			switch {
			case got == nil && c.want == nil:
				// ok
			case got == nil || c.want == nil:
				t.Errorf("got %v, want %v", got, c.want)
			case *got != *c.want:
				t.Errorf("got %d, want %d", *got, *c.want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

// --- classifyComplianceError ----------------------------------------------

func TestClassifyComplianceError_RequestBlockTakesPrecedence(t *testing.T) {
	req := &CompliancePipelineResult{Decision: RejectHard, Reason: "req-blocked"}
	resp := &CompliancePipelineResult{Decision: RejectHard, Reason: "resp-blocked"}
	code, reason := classifyComplianceError(req, resp, "", 200, nil)
	if code != "COMPLIANCE_BLOCKED" || reason != "req-blocked" {
		t.Errorf("got (%q,%q), want (COMPLIANCE_BLOCKED, req-blocked)", code, reason)
	}
}

func TestClassifyComplianceError_BlockSoftAlsoCounts(t *testing.T) {
	req := &CompliancePipelineResult{Decision: BlockSoft, Reason: "soft-block"}
	code, reason := classifyComplianceError(req, nil, "", 200, nil)
	if code != "COMPLIANCE_BLOCKED" || reason != "soft-block" {
		t.Errorf("got (%q,%q)", code, reason)
	}
}

func TestClassifyComplianceError_ResponseBlockWhenRequestApproves(t *testing.T) {
	req := &CompliancePipelineResult{Decision: Approve}
	resp := &CompliancePipelineResult{Decision: RejectHard, Reason: "resp-bad"}
	code, reason := classifyComplianceError(req, resp, "", 200, nil)
	if code != "COMPLIANCE_BLOCKED" || reason != "resp-bad" {
		t.Errorf("got (%q,%q)", code, reason)
	}
}

func TestClassifyComplianceError_BumpFailureBeforeProviderError(t *testing.T) {
	// BumpFailure must surface before the upstream HTTP error path —
	// otherwise a 502 from the upstream proxy would mask the more
	// useful TLS-inspection-unavailable signal.
	code, reason := classifyComplianceError(nil, nil, "BUMP_FAILED_PASSTHROUGH", 502, []byte("doesn't matter"))
	if code != "BUMP_FAILED" {
		t.Errorf("got code %q, want BUMP_FAILED", code)
	}
	if !strings.Contains(reason, "TLS inspection") {
		t.Errorf("reason: %q", reason)
	}
}

func TestClassifyComplianceError_ProviderErrorOn4xx5xx(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limited"}}`)
	code, reason := classifyComplianceError(nil, nil, "", 429, body)
	if code != "PROVIDER_ERROR" {
		t.Errorf("got code %q, want PROVIDER_ERROR", code)
	}
	if reason != "rate limited" {
		t.Errorf("reason: %q", reason)
	}
}

func TestClassifyComplianceError_NoErrorWhenAllApprove(t *testing.T) {
	req := &CompliancePipelineResult{Decision: Approve}
	resp := &CompliancePipelineResult{Decision: Approve}
	code, reason := classifyComplianceError(req, resp, "", 200, nil)
	if code != "" || reason != "" {
		t.Errorf("approve+200: got (%q,%q)", code, reason)
	}
}

// --- extractProviderErrorMessage -----------------------------------------

func TestExtractProviderErrorMessage_EmptyBodyFallsBackToStatus(t *testing.T) {
	got := extractProviderErrorMessage(nil, 503)
	if got != "provider returned HTTP 503" {
		t.Errorf("got %q", got)
	}
}

func TestExtractProviderErrorMessage_OpenAIShape(t *testing.T) {
	body := []byte(`{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`)
	if got := extractProviderErrorMessage(body, 429); got != "insufficient quota" {
		t.Errorf("got %q", got)
	}
}

func TestExtractProviderErrorMessage_TopLevelMessage(t *testing.T) {
	body := []byte(`{"message":"bad request"}`)
	if got := extractProviderErrorMessage(body, 400); got != "bad request" {
		t.Errorf("got %q", got)
	}
}

func TestExtractProviderErrorMessage_FallsBackToRawBody(t *testing.T) {
	body := []byte(`<html>upstream is down</html>`)
	got := extractProviderErrorMessage(body, 502)
	if got != "<html>upstream is down</html>" {
		t.Errorf("got %q", got)
	}
}

func TestExtractProviderErrorMessage_LongBodyTruncated(t *testing.T) {
	body := make([]byte, 400)
	for i := range body {
		body[i] = 'a'
	}
	got := extractProviderErrorMessage(body, 502)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("long body should end with ellipsis: %q", got[len(got)-20:])
	}
	if len(got) != 303 {
		t.Errorf("truncated length: %d, want 303 (300 + '...')", len(got))
	}
}

// --- headerLookup / extractUserAgent --------------------------------------

func TestHeaderLookup(t *testing.T) {
	h := map[string][]string{
		"Content-Type": {"application/json"},
		"X-Empty":      {},
	}
	if got := headerLookup(h, "Content-Type"); got != "application/json" {
		t.Errorf("got %q", got)
	}
	if got := headerLookup(h, "X-Empty"); got != "" {
		t.Errorf("empty slice should yield empty: %q", got)
	}
	if got := headerLookup(h, "X-Missing"); got != "" {
		t.Errorf("missing key should yield empty: %q", got)
	}
}

func TestExtractUserAgent_MissingReturnsNil(t *testing.T) {
	if got := extractUserAgent(map[string][]string{}); got != nil {
		t.Errorf("missing UA should yield nil: %v", got)
	}
}

func TestExtractUserAgent_EmptyValueReturnsNil(t *testing.T) {
	// Empty UA must yield nil (not pointer-to-empty) — analytics
	// uses IS NOT NULL semantics for the "saw a UA" count.
	if got := extractUserAgent(map[string][]string{"User-Agent": {""}}); got != nil {
		t.Errorf("empty UA should yield nil: %v", got)
	}
}

func TestExtractUserAgent_PresentReturnsPointer(t *testing.T) {
	h := map[string][]string{"User-Agent": {"curl/8.0"}}
	got := extractUserAgent(h)
	if got == nil {
		t.Fatal("present UA should yield non-nil pointer")
	}
	if *got != "curl/8.0" {
		t.Errorf("got %q", *got)
	}
}

func TestExtractUserAgent_TruncatesLongUA(t *testing.T) {
	// Wild Chrome/Edge UAs can hit 600+ chars; cap at 512 with
	// ellipsis marker so analytics can spot the cap.
	long := strings.Repeat("a", 800)
	got := extractUserAgent(map[string][]string{"User-Agent": {long}})
	if got == nil {
		t.Fatal("got nil")
	}
	if len(*got) != 512 {
		t.Errorf("truncated length: %d, want 512", len(*got))
	}
	if !strings.HasSuffix(*got, "...") {
		t.Errorf("truncation marker missing")
	}
}
