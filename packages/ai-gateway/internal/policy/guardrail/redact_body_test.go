package guardrail

import (
	"testing"

	"github.com/goccy/go-json"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func decodeRequest(t *testing.T, b []byte) Request {
	t.Helper()
	var r Request
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode redacted body: %v; body=%q", err, string(b))
	}
	return r
}

// TestRedactedRequestBody_ContentMasked is the storage half of the guardrail
// capture fix: under a redact or block verdict the storage gate persists ONLY
// this copy, so the sensitive span must be gone from it while the row still
// shows what was judged.
func TestRedactedRequestBody_ContentMasked(t *testing.T) {
	req := &Request{Content: "ping alice@example.com"}
	spans := []normalize.TransformSpan{{
		ContentAddress: "messages.0.content.0",
		Action:         normalize.ActionRedact,
		Start:          5,
		End:            22,
		Replacement:    "[REDACTED_EMAIL]",
	}}
	got := decodeRequest(t, RedactedRequestBody(req, spans))
	if got.Content != "ping [REDACTED_EMAIL]" {
		t.Errorf("content = %q, want %q", got.Content, "ping [REDACTED_EMAIL]")
	}
}

// TestRedactedRequestBody_MessagesKeepPositions pins the placement rule the
// direct block read exists for: masking a segment away must not shift the later
// segments onto the wrong message, because a mask on the wrong field would
// expose the very text it was meant to hide.
func TestRedactedRequestBody_MessagesKeepPositions(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "second alice@example.com"},
	}}
	// Segments() drops the empty message, so "second …" is segment index 1.
	spans := []normalize.TransformSpan{{
		ContentAddress: "messages.0.content.1",
		Action:         normalize.ActionRedact,
		Start:          7,
		End:            24,
		Replacement:    "[REDACTED_EMAIL]",
	}}
	got := decodeRequest(t, RedactedRequestBody(req, spans))

	want := []string{"first", "", "second [REDACTED_EMAIL]"}
	if len(got.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(want))
	}
	for i := range want {
		if got.Messages[i].Content != want[i] {
			t.Errorf("message[%d].content = %q, want %q — the empty message must not shift the mask",
				i, got.Messages[i].Content, want[i])
		}
	}
	if got.Messages[1].Role != "assistant" {
		t.Errorf("message[1].role = %q, want assistant preserved", got.Messages[1].Role)
	}
}

// TestRedactedRequestBody_NilWhenNothingToMask pins the fail-safe direction: when
// no trustworthy masked copy exists the answer is nil, which the caller stores as
// "no body" — never as a licence to store the raw request.
func TestRedactedRequestBody_NilWhenNothingToMask(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *Request
	}{
		{"nil request", nil},
		{"no segments", &Request{}},
		{"messages all empty", &Request{Messages: []Message{{Role: "user", Content: ""}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := RedactedRequestBody(tc.req, nil); out != nil {
				t.Errorf("got %q, want nil", string(out))
			}
		})
	}
}

// TestMaskedSegments_RejectsAForeignShape covers the guard that stops a mask
// landing on the wrong field when ApplySpans returns something other than the
// one-message, one-text-block-per-segment payload BuildNormalized handed it.
// Reached directly because BuildNormalized cannot produce these shapes — which is
// the point: the guard defends an invariant owned by another package.
func TestMaskedSegments_RejectsAForeignShape(t *testing.T) {
	text := func(s string) normalize.ContentBlock {
		return normalize.ContentBlock{Type: normalize.ContentText, Text: s}
	}
	for _, tc := range []struct {
		name string
		p    normalize.NormalizedPayload
		want int
	}{
		{"no messages", normalize.NormalizedPayload{}, 1},
		{"two messages", normalize.NormalizedPayload{Messages: []normalize.Message{
			{Content: []normalize.ContentBlock{text("a")}},
			{Content: []normalize.ContentBlock{text("b")}},
		}}, 1},
		{"block count changed", normalize.NormalizedPayload{Messages: []normalize.Message{
			{Content: []normalize.ContentBlock{text("a")}},
		}}, 2},
		{"block retyped", normalize.NormalizedPayload{Messages: []normalize.Message{
			{Content: []normalize.ContentBlock{{Type: normalize.ContentMedia}}},
		}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskedSegments(tc.p, tc.want); got != nil {
				t.Errorf("got %q, want nil — a shape this function cannot place must not be placed", got)
			}
		})
	}
}

// TestMaskedSegments_ReadsEveryBlock covers the accepting path, including a
// segment masked to the empty string — the case TextProjection would have
// dropped.
func TestMaskedSegments_ReadsEveryBlock(t *testing.T) {
	p := normalize.NormalizedPayload{Messages: []normalize.Message{{Content: []normalize.ContentBlock{
		{Type: normalize.ContentText, Text: "kept"},
		{Type: normalize.ContentText, Text: ""},
		{Type: normalize.ContentText, Text: "also kept"},
	}}}}
	got := maskedSegments(p, 3)
	want := []string{"kept", "", "also kept"}
	if len(got) != len(want) {
		t.Fatalf("got %d segments, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
