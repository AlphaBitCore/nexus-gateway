package canonicalbridge_test

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The existing Cohere identity test compares the TEXT each channel carries. That
// cannot see the block structure around the text, and the block structure is
// what a client uses to know when a block is finished: Cohere clients finalize
// on `content-end`.
//
// Re-encoding got all three structural facts wrong at once — the thinking block
// was numbered 1 and the text block 0 (upstream numbers them the other way, in
// the order they open), the two blocks overlapped where upstream closes each
// before opening the next, and `content-end` for the thinking block was never
// emitted at all, so a client waiting for it waits forever.
//
// The gate compares the open/close skeleton against the captured upstream,
// event for event.

// cohereBlockSkeleton reduces a Cohere SSE stream to its block structure:
// content-start / content-end in order, with the index and declared type. Deltas
// are dropped — their text is already covered by the channel comparison, and
// including them would make a failure here unreadable.
func cohereBlockSkeleton(t *testing.T, sse string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(sse))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		ev := gjson.Parse(strings.TrimSpace(payload))
		switch ev.Get("type").Str {
		case "content-start":
			kind := ev.Get("delta.message.content.type").Str
			if kind == "" {
				kind = "text"
			}
			out = append(out, fmt.Sprintf("start[%d]:%s", ev.Get("index").Int(), kind))
		case "content-end":
			out = append(out, fmt.Sprintf("end[%d]", ev.Get("index").Int()))
		}
	}
	return out
}

func TestCohereBlockStructureMatchesUpstream(t *testing.T) {
	raw := readCorpus(t, "cohere_reasoning")
	tc := fidelityCase{
		corpus: "cohere_reasoning", ingress: provcore.FormatCohere,
		shape: typology.WireShapeCohereChat, open: cohereOpen,
	}
	_, wire := runChain(t, tc, raw)

	want := cohereBlockSkeleton(t, raw)
	got := cohereBlockSkeleton(t, wire)

	if len(want) == 0 {
		t.Fatal("the capture carries no content blocks; this gate has no subject")
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("block structure differs from the captured upstream.\n"+
			"  upstream:  %s\n  re-encode: %s\n"+
			"A Cohere client finalizes a block on content-end and reads its type from "+
			"content-start; a block that never closes never finalizes, and swapped indices "+
			"attribute the thinking to the answer.",
			strings.Join(want, " "), strings.Join(got, " "))
	}
}

// TestCohereBlocksDoNotOverlap states the invariant independently of the
// capture, so it keeps holding for shapes the corpus does not contain: a block
// must be closed before the next one opens, and indices ascend in open order.
func TestCohereBlocksDoNotOverlap(t *testing.T) {
	raw := readCorpus(t, "cohere_reasoning")
	tc := fidelityCase{
		corpus: "cohere_reasoning", ingress: provcore.FormatCohere,
		shape: typology.WireShapeCohereChat, open: cohereOpen,
	}
	_, wire := runChain(t, tc, raw)

	open := -1
	next := 0
	for _, ev := range cohereBlockSkeleton(t, wire) {
		var idx int
		switch {
		case strings.HasPrefix(ev, "start["):
			fmt.Sscanf(ev, "start[%d]", &idx)
			if open >= 0 {
				t.Fatalf("%s opened while block %d was still open — upstream closes each block "+
					"before opening the next", ev, open)
			}
			if idx != next {
				t.Fatalf("%s does not continue the ascending order (expected index %d); a client "+
					"keying blocks by index cannot line them up", ev, next)
			}
			open, next = idx, next+1
		case strings.HasPrefix(ev, "end["):
			fmt.Sscanf(ev, "end[%d]", &idx)
			if idx != open {
				t.Fatalf("%s closes a block that is not the open one (%d)", ev, open)
			}
			open = -1
		}
	}
	if open >= 0 {
		t.Errorf("block %d was never closed; a client that finalizes on content-end waits for an "+
			"event that never arrives", open)
	}
}
