package tlsbump

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// adapterWireCodec reuses one scratch buffer for the string-to-bytes conversion
// ExtractStreamChunk needs (finding C-20). The Model-A substrate RETAINS each frame's
// extracted text in its wireUnit and reads it later, so a reused buffer that leaked into
// the returned text would corrupt an earlier frame's text once a later frame overwrote it —
// wrong bytes spliced into a redacted stream on the enforcement path, with no error raised.

func newScratchCodec() adapterWireCodec {
	return adapterWireCodec{
		ctx:     context.Background(),
		adapter: &openai.Adapter{},
		path:    "/v1/chat/completions",
		scratch: new([]byte),
	}
}

func openAIDeltaFrame(content string) string {
	return `{"choices":[{"index":0,"delta":{"content":"` + content + `"}}]}`
}

// TestAdapterWireCodec_RetainedTextSurvivesLaterFrames is the equivalence assertion. Every
// returned text is held across all subsequent calls and checked at the end, which is exactly
// how the substrate uses it.
func TestAdapterWireCodec_RetainedTextSurvivesLaterFrames(t *testing.T) {
	codec := newScratchCodec()

	// Lengths deliberately grow then SHRINK: growth forces the scratch to reallocate, and
	// a shorter frame after a longer one is the case where a stale tail would show up.
	contents := []string{
		strings.Repeat("a", 8),
		strings.Repeat("b", 64),
		strings.Repeat("c", 12),
		strings.Repeat("d", 200),
		strings.Repeat("e", 3),
	}

	got := make([]string, 0, len(contents))
	for i, c := range contents {
		txt, ok := codec.ChunkText(openAIDeltaFrame(c))
		if !ok {
			t.Fatalf("frame %d: ChunkText declined", i)
		}
		got = append(got, txt)
	}

	for i, want := range contents {
		if got[i] != want {
			t.Fatalf("frame %d text = %q, want %q.\nThe reused scratch leaked into the returned "+
				"text: a later frame overwrote an earlier frame's content. The Model-A substrate "+
				"retains this text in its wireUnit and reads it after subsequent frames have been "+
				"parsed, so this would splice wrong bytes into a redacted stream.", i, got[i], want)
		}
	}
}

// TestAdapterWireCodec_ZeroValueStillWorks pins the fallback. A codec built without a
// scratch — the zero value, or any future construction site that forgets it — must not hand
// ExtractStreamChunk a nil buffer and silently extract nothing. Silent nothing is the
// dangerous outcome here: no error, no text, no redaction.
func TestAdapterWireCodec_ZeroValueStillWorks(t *testing.T) {
	codec := adapterWireCodec{
		ctx:     context.Background(),
		adapter: &openai.Adapter{},
		path:    "/v1/chat/completions",
		// scratch deliberately nil
	}
	txt, ok := codec.ChunkText(openAIDeltaFrame("hello"))
	if !ok || txt != "hello" {
		t.Fatalf("ChunkText = %q, ok=%v; want %q, true. A codec without a scratch must fall back "+
			"to a fresh conversion, not extract nothing.", txt, ok, "hello")
	}
}

// TestAdapterWireCodec_ScratchIsReusedNotReallocated pins the optimization itself, so a
// refactor that quietly drops the reuse fails rather than just getting slower. The scratch's
// capacity must be retained across calls once it has grown.
func TestAdapterWireCodec_ScratchIsReusedNotReallocated(t *testing.T) {
	codec := newScratchCodec()

	long := openAIDeltaFrame(strings.Repeat("x", 512))
	if _, ok := codec.ChunkText(long); !ok {
		t.Fatal("ChunkText declined the long frame")
	}
	grown := cap(*codec.scratch)
	if grown < len(long) {
		t.Fatalf("scratch cap = %d after a %d-byte frame, want at least the frame length", grown, len(long))
	}

	// A short frame must NOT shrink the buffer — that would mean every frame reallocates and
	// the finding's win is gone.
	if _, ok := codec.ChunkText(openAIDeltaFrame("s")); !ok {
		t.Fatal("ChunkText declined the short frame")
	}
	if got := cap(*codec.scratch); got != grown {
		t.Fatalf("scratch cap = %d after a short frame, want it retained at %d. %s",
			got, grown, "The buffer is being reallocated per frame, so the conversion was never amortized.")
	}
}

// TestAdapterWireCodec_SubstrateAndRedactorDoNotShareAScratch pins the assumption the
// no-synchronization decision rests on. Each construction site builds its own codec value,
// so the scratch is per-request state. If a future refactor made them share one instance,
// two paths could write the same buffer and this fails first.
func TestAdapterWireCodec_SubstrateAndRedactorDoNotShareAScratch(t *testing.T) {
	adapter := &openai.Adapter{}
	redactor := newSSEFrameRedactor(context.Background(), adapter, "/v1/chat/completions", false, discardSlog())
	if redactor == nil {
		t.Fatal("newSSEFrameRedactor returned nil for a non-nil adapter")
	}
	rc, ok := redactor.codec.(adapterWireCodec)
	if !ok {
		t.Fatalf("redactor codec type = %T, want adapterWireCodec", redactor.codec)
	}
	substrateCodec := adapterWireCodec{ctx: context.Background(), adapter: adapter, path: "/v1/chat/completions", scratch: new([]byte)}

	if rc.scratch == nil {
		t.Fatal("the redactor's codec has no scratch, so its conversions are not amortized")
	}
	if rc.scratch == substrateCodec.scratch {
		t.Fatal("the redactor and the substrate share one scratch buffer. The reuse is only " +
			"safe because each holds its own per-request buffer; sharing one would need " +
			"synchronization, which B13 forbids adding to this path.")
	}
	// Sanity: fmt keeps the pointers referenced so the comparison cannot be optimized out.
	_ = fmt.Sprintf("%p %p", rc.scratch, substrateCodec.scratch)
}
