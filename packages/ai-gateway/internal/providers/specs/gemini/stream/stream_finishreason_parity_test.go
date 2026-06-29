package stream

// Internal test (package stream) so it can reach the unexported
// mapGeminiFinishToCanonical. The stream package INLINES this mapping to
// avoid a production import cycle with gemini/codec; this test-only import is
// the only thing that keeps the inlined copy in lockstep with the source of
// truth (codec.MapFinishReason). If either side changes without the other,
// this fails — closing the silent-drift hole.

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
)

func TestMapGeminiFinishToCanonical_ParityWithCodec(t *testing.T) {
	for _, r := range []string{
		"STOP", "MAX_TOKENS", "SAFETY", "RECITATION", "LANGUAGE",
		"PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "IMAGE_SAFETY",
		"MODEL_ARMOR", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL",
		"OTHER", "", "FUTURE_UNKNOWN",
	} {
		got := mapGeminiFinishToCanonical(r)
		want := codec.MapFinishReason(r)
		if got != want {
			t.Errorf("drift: mapGeminiFinishToCanonical(%q)=%q but codec.MapFinishReason(%q)=%q", r, got, r, want)
		}
	}
}
