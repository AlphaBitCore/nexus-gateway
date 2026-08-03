package adapters

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// ExtractStreamChunk must not return segments that alias its input buffer.
//
// This is a CONTRACT test, not a coverage test, and it is the precondition for finding
// C-20's remaining optimization: the SSE Model-A path converts every frame's Data string
// to []byte for ExtractStreamChunk, once per frame, and the only way to amortize that is
// to reuse one scratch buffer across frames. Reuse is safe only if no adapter hands back a
// string that points into the buffer it was given.
//
// The hazard is not hypothetical, and it is not obvious from the call site either:
// tlsbump's adapterWireCodec does strings.Join(nc.Segments, ""), and Go's strings.Join
// returns the element UNCHANGED for a single-element slice — which is the common case for
// one frame. So a single aliasing segment becomes the returned text verbatim, and that text
// is stored in the wireUnit and read later, after the next frame has already overwritten
// the buffer. The symptom would be wrong bytes spliced into a redacted stream on the
// enforcement path, with no error raised anywhere.
//
// Any adapter that decodes with unsafe, or retains a gjson Result.Raw slice, or otherwise
// builds a string without copying, fails here. That is the intended outcome: it tells
// whoever adds it that the streaming redaction path cannot reuse its buffer.

// aliasProbeFrames are payload shapes spanning the provider families the built-in adapters
// cover, so several adapters actually decode rather than all declining. An adapter that
// declines every shape contributes nothing and is reported, not silently counted.
var aliasProbeFrames = []struct {
	name string
	path string
	body string
}{
	{
		name: "openai-chat-delta",
		path: "/v1/chat/completions",
		body: `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello there"}}]}`,
	},
	{
		name: "openai-chat-multi-choice",
		path: "/v1/chat/completions",
		body: `{"choices":[{"index":0,"delta":{"content":"aa"}},{"index":1,"delta":{"content":"bb"}}]}`,
	},
	{
		name: "anthropic-content-block-delta",
		path: "/v1/messages",
		body: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello there"}}`,
	},
	{
		name: "gemini-generate-chunk",
		path: "/v1beta/models/gemini-2.5-pro:streamGenerateContent",
		body: `{"candidates":[{"content":{"parts":[{"text":"hello there"}],"role":"model"}}]}`,
	},
	{
		name: "openai-responses-delta",
		path: "/v1/responses",
		body: `{"type":"response.output_text.delta","delta":"hello there"}`,
	},
}

func TestExtractStreamChunk_SegmentsNeverAliasTheInput(t *testing.T) {
	reg := traffic.NewAdapterRegistry("alias-contract-test")
	RegisterBuiltins(reg)

	ids := reg.All()
	if len(ids) == 0 {
		t.Fatal("no built-in adapters registered — the test would pass vacuously")
	}

	exercised := 0
	for _, id := range ids {
		factory := reg.Get(id)
		if factory == nil {
			t.Fatalf("registry lists %q but Get returned nil", id)
		}
		for _, f := range aliasProbeFrames {
			adapter := factory()

			buf := []byte(f.body)
			nc, err := adapter.ExtractStreamChunk(context.Background(), buf, f.path)
			if err != nil || len(nc.Segments) == 0 {
				continue // this adapter does not model this shape; nothing to check
			}

			// Keep an independent copy of what was returned, then destroy the input.
			before := make([]string, len(nc.Segments))
			copy(before, nc.Segments)
			for i := range buf {
				buf[i] = 0xFF
			}

			for i, seg := range nc.Segments {
				if seg != before[i] {
					t.Errorf("adapter %q, frame %q: segment %d changed after the input buffer was "+
						"overwritten (%q -> %q).\n"+
						"The segment aliases the input, so the SSE Model-A path cannot reuse one "+
						"scratch buffer across frames (finding C-20): strings.Join returns a "+
						"single segment unchanged, that text is retained in the wireUnit, and the "+
						"next frame's copy would corrupt it — wrong bytes spliced into a redacted "+
						"stream, with no error raised. Copy the bytes out before building the "+
						"segment.",
						id, f.name, i, before[i], seg)
				}
			}
			exercised++
		}
	}

	// Guard against a vacuous pass. If every adapter declined every probe shape, the loop
	// above asserts nothing at all — which is how a contract test rots into decoration.
	if exercised < 4 {
		t.Fatalf("only %d adapter/frame combinations decoded a segment, want at least 4. "+
			"Either the probe payloads no longer match any adapter's wire shape, or adapter "+
			"registration changed — either way this test is no longer checking the contract "+
			"it exists for. Fix the payloads rather than lowering this bound.", exercised)
	}
	t.Logf("non-aliasing contract verified across %d adapter/frame combinations "+
		"(%d built-in adapters probed)", exercised, len(ids))
}
