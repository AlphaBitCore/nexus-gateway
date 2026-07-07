package canonicalbridge

import (
	"context"
	"testing"

	json "github.com/goccy/go-json"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// referenceContentFrame produces the content-delta frame the way the struct
// encoder would (emit(oaiStreamChoice{Delta:{Content:&c}}, nil)) — the byte
// baseline the fast path must match.
func referenceContentFrame(e *openAIStreamEncoder, content string) string {
	cc := content
	data, _ := json.Marshal(oaiStreamEnvelope{
		Choices: []oaiStreamChoice{{Delta: oaiStreamDelta{Content: &cc}}},
		Created: e.created,
		ID:      e.id,
		Model:   e.model,
		Object:  "chat.completion.chunk",
	})
	return "data: " + string(data) + "\n\n"
}

// TestOpenAIStreamEncoder_ContentDeltaFastPath pins the per-token fast path to be
// byte-identical to the struct-marshalled envelope for a range of content that
// exercises JSON string escaping (quotes, backslash, control chars, HTML-unsafe
// chars, line/paragraph separators, multibyte, and would-be JSON-injection).
func TestOpenAIStreamEncoder_ContentDeltaFastPath(t *testing.T) {
	cases := []string{
		"",
		"hello world",
		`a "quoted" b`,
		`back\slash`,
		"line\nbreak\r\ttab",
		"html <b>tag</b> & ampersand",
		"café 日本語 😀 é",
		"line sep para",
		"ctrl\x00\x01\x1f end",
		`"},"finish_reason":"injected"}]}`, // must be escaped, not break the frame
		"trailing backslash \\",
	}
	for _, c := range cases {
		e := newOpenAIStreamEncoder("gpt-4o")
		e.scratch = e.scratch[:0]
		e.emitContentDelta(c)
		got := string(e.scratch)
		want := referenceContentFrame(e, c)
		if got != want {
			t.Errorf("content %q:\n got=%q\nwant=%q", c, got, want)
		}
		// The fast-path output must parse as valid JSON with the right content.
		var env struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		payload := got[len("data: ") : len(got)-2]
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			t.Fatalf("content %q: fast-path frame is not valid JSON: %v (%q)", c, err, payload)
		}
		if len(env.Choices) != 1 || env.Choices[0].Delta.Content != c {
			t.Errorf("content %q: round-trip mismatch, got %q", c, env.Choices[0].Delta.Content)
		}
	}
}

// FuzzOpenAIStreamEncoder_ContentDelta asserts byte-identity of the fast path vs
// the struct encoder for arbitrary content, so no escaping edge case can diverge.
func FuzzOpenAIStreamEncoder_ContentDelta(f *testing.F) {
	for _, s := range []string{"", "x", `"`, "\\", "<&>", "😀", " ", "\x00", `"}]}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, content string) {
		e := newOpenAIStreamEncoder("model-x")
		e.scratch = e.scratch[:0]
		e.emitContentDelta(content)
		got := string(e.scratch)
		want := referenceContentFrame(e, content)
		if got != want {
			t.Fatalf("content %q:\n got=%q\nwant=%q", content, got, want)
		}
	})
}

// TestOpenAIStreamEncoder_WriteContentUnchanged verifies the full Write path still
// produces the exact same bytes for a content chunk after routing it through the
// fast path (the role-header frame precedes it on the first Write, and stays on
// the struct-marshal path).
func TestOpenAIStreamEncoder_WriteContentUnchanged(t *testing.T) {
	e := newOpenAIStreamEncoder("gpt-4o")
	c := `hi "there" <world>`
	out, err := e.Write(context.Background(), provcore.Chunk{Delta: c})
	if err != nil {
		t.Fatal(err)
	}
	// First Write emits the role-header frame (struct path) then the content
	// frame (fast path); both reference the same encoder id/created, unchanged by
	// Write.
	empty := ""
	roleData, _ := json.Marshal(oaiStreamEnvelope{
		Choices: []oaiStreamChoice{{Delta: oaiStreamDelta{Content: &empty, Role: "assistant"}}},
		Created: e.created,
		ID:      e.id,
		Model:   e.model,
		Object:  "chat.completion.chunk",
	})
	want := "data: " + string(roleData) + "\n\n" + referenceContentFrame(e, c)
	if string(out) != want {
		t.Errorf("Write content frame changed:\n got=%q\nwant=%q", string(out), want)
	}
}

// BenchmarkOpenAIStreamEncoder_ContentDelta is the before/after comparison: the
// per-token fast path vs the struct-marshalled envelope path.
func BenchmarkOpenAIStreamEncoder_ContentDelta(b *testing.B) {
	content := "The quick brown fox jumps over the lazy dog, then keeps on going."
	b.Run("fast", func(b *testing.B) {
		e := newOpenAIStreamEncoder("gpt-4o")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			e.scratch = e.scratch[:0]
			e.emitContentDelta(content)
		}
	})
	b.Run("marshal", func(b *testing.B) {
		e := newOpenAIStreamEncoder("gpt-4o")
		cc := content
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			e.scratch = e.scratch[:0]
			e.emit(oaiStreamChoice{Delta: oaiStreamDelta{Content: &cc}}, nil)
		}
	})
}
