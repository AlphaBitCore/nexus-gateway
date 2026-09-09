package redact

import (
	"strings"
	"testing"
)

// allowRawQuote governs exactly one of ProviderErrorMessage's three branches,
// and getting that boundary wrong is expensive in both directions.
//
// Too permissive and an operator who turned payload capture off finds the
// excluded response body in traffic_event.error_reason, the column beside the
// one the gate protects. Too strict — which a first attempt at this was, by
// nilling the body instead of passing the permission — and every upstream
// failure on the DEFAULT configuration reads "provider returned HTTP 400",
// because payloadcapture.DefaultConfig() has StoreResponseBody false.

// wafPage is the shape that reaches the raw branch: not JSON, so neither
// structured lookup matches. Upstream WAF and CDN pages routinely quote the
// caller's own request back, which is why this branch is the one under policy.
const wafPage = `<html><head><title>403 Forbidden</title></head><body>` +
	`Request blocked. Offending header: Authorization: Bearer sk-live-CALLER-SECRET` +
	`</body></html>`

// TestProviderErrorMessage_StructuredMessageSurvivesWithoutRawQuote is the
// regression guard. The provider's own diagnostic is a bounded, provider-
// authored string — it is what an operator triages a 4xx with, and it is not a
// stored response body in any sense the payload policy is about.
func TestProviderErrorMessage_StructuredMessageSurvivesWithoutRawQuote(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			"openai / anthropic / gemini envelope",
			`{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`,
			"You exceeded your current quota",
		},
		{
			"top-level message",
			`{"message":"model_not_found"}`,
			"model_not_found",
		},
		{
			"the message an operator most needs",
			`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`,
			"This model's maximum context length is 8192 tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderErrorMessage([]byte(tc.body), 429, false); got != tc.want {
				t.Fatalf("the provider's own diagnostic was lost with raw quoting off.\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestProviderErrorMessage_RawBodyIsWithheldWithoutPermission is the other
// direction: the branch that copies arbitrary upstream bytes stays shut.
func TestProviderErrorMessage_RawBodyIsWithheldWithoutPermission(t *testing.T) {
	got := ProviderErrorMessage([]byte(wafPage), 403, false)

	if strings.Contains(got, "CALLER-SECRET") || strings.Contains(got, "<html>") {
		t.Fatalf("response-body bytes reached error_reason with raw quoting off: %q", got)
	}
	if got != "provider returned HTTP 403" {
		t.Fatalf("expected the status line, got %q — it must still say what happened", got)
	}
}

// And with permission the raw branch still works, because a caller holding
// bytes the storage gate already approved loses nothing by quoting them.
func TestProviderErrorMessage_RawBodyQuotedWithPermission(t *testing.T) {
	got := ProviderErrorMessage([]byte(wafPage), 403, true)

	if !strings.Contains(got, "403 Forbidden") {
		t.Fatalf("with permission the raw body must still be quoted, got %q", got)
	}
	if len(got) > MaxErrorReasonBytes+3 {
		t.Fatalf("the quote must stay bounded: %d bytes", len(got))
	}
}

// The empty-body branch is unaffected by the permission — there is nothing to
// quote either way, and both must give the operator the status line.
func TestProviderErrorMessage_EmptyBodyIgnoresThePermission(t *testing.T) {
	for _, allow := range []bool{true, false} {
		if got := ProviderErrorMessage(nil, 503, allow); got != "provider returned HTTP 503" {
			t.Fatalf("allowRawQuote=%v: got %q", allow, got)
		}
	}
}
