package format

import (
	"strings"
	"testing"
)

func TestExtractDeltaText(t *testing.T) {
	tests := []struct {
		data string
		want string
	}{
		{`{"choices":[{"delta":{"content":"Hello"}}]}`, "Hello"},
		{`{"choices":[{"delta":{"role":"assistant"}}]}`, ""},
		{`{"choices":[]}`, ""},
		{`not json`, ""},
	}
	for _, tt := range tests {
		if got := ExtractDeltaText(tt.data); got != tt.want {
			t.Errorf("ExtractDeltaText(%q) = %q, want %q", tt.data, got, tt.want)
		}
	}
}

func TestOpenAIStreamDeltaPayload(t *testing.T) {
	// Happy path: returns a parseable JSON string carrying the content
	// in the OpenAI delta envelope. Used by LivePipeline's Modify
	// branch to replace held-back assistant deltas.
	out, err := OpenAIStreamDeltaPayload("rewritten text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"content":"rewritten text"`) {
		t.Errorf("expected content in delta payload, got %q", out)
	}
	if !strings.Contains(out, `"choices"`) {
		t.Errorf("expected choices envelope, got %q", out)
	}
	if !strings.Contains(out, `"index":0`) {
		t.Errorf("expected index:0, got %q", out)
	}
}

// TestOpenAIStreamDeltaPayload_RoundTripsThroughExtract verifies the
// OpenAI delta envelope produced by OpenAIStreamDeltaPayload is the
// SAME wire shape ExtractDeltaText knows how to parse — a Modify
// payload must be re-readable as a valid SSE chunk by downstream
// consumers and by re-entry into LivePipeline.
func TestOpenAIStreamDeltaPayload_RoundTripsThroughExtract(t *testing.T) {
	payload, err := OpenAIStreamDeltaPayload("round-trip me")
	if err != nil {
		t.Fatal(err)
	}
	got := ExtractDeltaText(payload)
	if got != "round-trip me" {
		t.Errorf("round-trip mismatch: ExtractDeltaText returned %q, want %q", got, "round-trip me")
	}
}
