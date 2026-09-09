package proxy

import (
	"context"
	"strings"
	"testing"

	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

const applyBody = `{"id":"a","object":"chat.completion","model":"gpt-4o",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"call 555-0100"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func decodeApplyBody(t *testing.T) *normcore.NormalizedPayload {
	t.Helper()
	p, err := normcodecs.SharedOpenAIChat().Normalize(context.Background(), []byte(applyBody), normcore.Meta{
		AdapterType:  "openai",
		Direction:    normcore.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &p
}

// TestApplyCanonicalRedaction_AppliesSpanToTheContentChannel is the positive
// case the two refusals below are measured against.
func TestApplyCanonicalRedaction_AppliesSpanToTheContentChannel(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	out, n, err := h.applyCanonicalRedaction([]byte(applyBody),
		decodeApplyBody(t), []normcore.TransformSpan{{
			Source:         normcore.SourceHook,
			Action:         normcore.ActionRedact,
			ContentAddress: "messages.0.content.0",
			Start:          5,
			End:            13,
			Replacement:    "[REDACTED]",
		}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Errorf("wrote %d slots, want 1", n)
	}
	if !strings.Contains(string(out), "[REDACTED]") || strings.Contains(string(out), "555-0100") {
		t.Errorf("body=%s — the span should have masked the phone number in place", string(out))
	}
	if !strings.Contains(string(out), `"total_tokens":2`) {
		t.Errorf("body=%s — the envelope must survive a content redaction untouched", string(out))
	}
}

// TestApplyCanonicalRedaction_RefusesWithoutAPayload covers the arm that makes
// the locus fail-closed: the policy demanded a redaction and there is nothing to
// apply it to, so the response must not go out.
func TestApplyCanonicalRedaction_RefusesWithoutAPayload(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	_, _, err := h.applyCanonicalRedaction([]byte(applyBody), nil,
		[]normcore.TransformSpan{{ContentAddress: "messages.0.content.0", End: 1, Replacement: "x"}})
	if err == nil {
		t.Fatal("a redaction with no canonical payload was allowed to proceed; the original body " +
			"would have been delivered with an approve stamp on it")
	}
	if !strings.Contains(err.Error(), "no canonical payload") {
		t.Errorf("err=%v — the message should name what was missing", err)
	}
}
