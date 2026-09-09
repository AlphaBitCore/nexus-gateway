// Package stream — the per-frame decode benchmark.
//
// chatChunkFromFrame is the hottest application function in the streaming path:
// on a 1000 rps production profile `openaiStreamSession.Next` accounted for
// ~25% of CPU, of which this function was ~15%, essentially all of it inside
// gjson. Every measurement here runs over frames captured from real upstreams,
// because frame SHAPE decides the cost — how many keys the delta carries, and
// whether the walk short-circuits — and an invented frame measures an invented
// shape.
package stream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// streamCorpusDir holds the captured SSE streams the fidelity gates run over.
const streamCorpusDir = "../../../../execution/canonicalbridge/testdata/upstream-streams"

// framesFromCorpus returns every data frame of a captured stream, in order.
func framesFromCorpus(tb testing.TB, name string) []specutil.SSEEvent {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(streamCorpusDir, name+".response.sse")))
	if err != nil {
		tb.Fatalf("read stream corpus %q: %v", name, err)
	}
	var out []specutil.SSEEvent
	for _, block := range strings.Split(string(raw), "\n\n") {
		var ev specutil.SSEEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.Data = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(ev.Data) == 0 || string(ev.Data) == "[DONE]" {
			continue
		}
		out = append(out, ev)
	}
	if len(out) == 0 {
		tb.Fatalf("stream corpus %q yielded no data frames", name)
	}
	return out
}

// BenchmarkChatChunkFromFrame decodes each corpus in turn. The corpora differ in
// the one dimension that drives cost — a plain text delta walks the smallest
// delta object, a tool-call delta walks a nested array, and a reasoning stream
// carries the extra channel — so a change that speeds one shape at another's
// expense shows up as a split rather than as a wash.
func BenchmarkChatChunkFromFrame(b *testing.B) {
	for _, corpus := range []string{
		"openai_multichoice",
		"openai_tools",
		"openai_refusal",
		"deepseek_reasoning",
	} {
		frames := framesFromCorpus(b, corpus)
		b.Run(corpus, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(totalBytes(frames)))
			b.ResetTimer()
			for range b.N {
				for _, ev := range frames {
					_ = chatChunkFromFrame(ev)
				}
			}
		})
	}
}

func totalBytes(frames []specutil.SSEEvent) int {
	n := 0
	for _, f := range frames {
		n += len(f.Data)
	}
	return n
}
