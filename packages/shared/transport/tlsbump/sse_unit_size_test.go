package tlsbump

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	trafficanthropic "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
	trafficcohere "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/cohere"
	trafficgemini "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/gemini"
	trafficopenai "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/modela"
)

// The Model-A soundness condition is
//
//	window > maxPattern + prescanBatch + maxUnitSize
//
// and TailWindowFor sizes the window with exactly ONE PrescanBatchBytes of
// headroom for that third term, on the stated reasoning that a unit up to a full
// prescan batch is "already orders of magnitude above the one-token frame a chat
// stream produces". That is a claim about real provider traffic, and it was
// reasoning, not a measurement — the kind that is true until a provider ships a
// chunkier stream and nothing notices.
//
// This measures it against every streaming corpus in the repo, through the
// SUBSTRATE'S OWN extractor rather than a re-implementation of it. What grows
// the scan buffer is `AppendRedactableText`, which appends `codec.ChunkText`,
// which is `strings.Join(adapter.ExtractStreamChunk(...).Segments, "")` — so
// that is what gets measured. Counting raw frame bytes instead would report a
// number the engine never sees: the largest raw event in these corpora is over
// 4KB while the largest extracted text is a fraction of that.
func TestSSEUnitTextFitsTheTailWindowHeadroom(t *testing.T) {
	corpusDir := filepath.Join("..", "..", "..", "ai-gateway", "internal",
		"execution", "canonicalbridge", "testdata", "upstream-streams")

	// The adapter is chosen by the corpus's provider prefix, because the
	// extraction is per-wire and a Gemini frame read by the OpenAI adapter
	// yields nothing — which would report a comfortable zero.
	adapterFor := func(name string) (traffic.Adapter, string) {
		switch {
		case strings.HasPrefix(name, "anthropic"):
			return &trafficanthropic.Adapter{}, "/v1/messages"
		case strings.HasPrefix(name, "cohere"):
			return &trafficcohere.Adapter{}, "/v2/chat"
		case strings.HasPrefix(name, "gemini"):
			return &trafficgemini.Adapter{}, "/v1beta/models/x:streamGenerateContent"
		case strings.HasPrefix(name, "openai_responses"):
			return &trafficopenai.Adapter{}, "/v1/responses"
		default:
			// deepseek / moonshot / openai — all the OpenAI chat SSE wire.
			return &trafficopenai.Adapter{}, "/v1/chat/completions"
		}
	}

	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read the streaming corpora at %s: %v", corpusDir, err)
	}

	eventSplit := regexp.MustCompile(`\n\s*\n`)
	headroom := modela.DefaultPrescanBatchBytes

	corpora, worst, worstIn := 0, 0, ""
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".response.sse") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".response.sse")
		raw, rerr := os.ReadFile(filepath.Join(corpusDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		adapter, path := adapterFor(name)

		frames, maxText := 0, 0
		for _, block := range eventSplit.Split(string(raw), -1) {
			var data []string
			for _, line := range strings.Split(block, "\n") {
				if after, ok := strings.CutPrefix(line, "data:"); ok {
					data = append(data, strings.TrimSpace(after))
				}
			}
			if len(data) == 0 {
				continue
			}
			frames++
			payload := strings.Join(data, "\n")
			nc, xerr := adapter.ExtractStreamChunk(context.Background(), []byte(payload), path)
			if xerr != nil {
				continue // a frame the adapter cannot read contributes nothing
			}
			if n := len(strings.Join(nc.Segments, "")); n > maxText {
				maxText = n
			}
		}
		if frames == 0 {
			t.Errorf("%s produced no SSE frames — the corpus or the split changed shape, and "+
				"a corpus that yields nothing reports a comfortable zero", name)
			continue
		}
		corpora++
		t.Logf("%-32s frames=%-5d max extracted text=%d bytes", name, frames, maxText)
		if maxText > worst {
			worst, worstIn = maxText, name
		}
	}

	if corpora < 10 {
		t.Fatalf("only %d corpora measured — this gate is meant to sweep the whole set, and "+
			"a shrunken sweep reports clean for the wrong reason", corpora)
	}
	// The extractors must actually be extracting. All-zero would satisfy the
	// bound below while proving nothing at all about it.
	if worst == 0 {
		t.Fatalf("every corpus extracted zero text across %d files — the adapters are not "+
			"reading these wires and the headroom check below is vacuous", corpora)
	}
	t.Logf("worst extracted unit: %d bytes (%s); headroom bought by TailWindowFor: %d",
		worst, worstIn, headroom)

	if worst >= headroom {
		t.Errorf("the largest single unit in the corpora is %d bytes (%s), at or past the %d "+
			"bytes of headroom TailWindowFor leaves for maxUnitSize. The soundness condition "+
			"window > maxPattern + prescanBatch + maxUnitSize no longer holds by construction, "+
			"so a pattern straddling that unit's boundary can leak a bounded fragment. Size the "+
			"headroom from this measurement rather than from the one-token-frame assumption.",
			worst, worstIn, headroom)
	}
}
