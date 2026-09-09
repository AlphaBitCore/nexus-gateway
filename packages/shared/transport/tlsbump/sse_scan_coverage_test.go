package tlsbump

import (
	"context"
	"encoding/json"
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
)

// What a streaming compliance scan can see is decided by one expression —
// `codec.ChunkText`, which is what `AppendRedactableText` appends. A wire whose
// text that expression cannot reach is delivered to the client unscanned, and
// nothing about the delivery looks different: no error, no counter, no log. The
// two defects this file guards were both invisible in exactly that way for as
// long as they existed.
//
// The corpora are captured upstream streams, so a provider changing its wire
// shows up here rather than in production.

func scanCoverageCorpusDir() string {
	return filepath.Join("..", "..", "..", "ai-gateway", "internal",
		"execution", "canonicalbridge", "testdata", "upstream-streams")
}

// scanCoverageAdapterFor picks the wire's own adapter. A Gemini frame read by
// the OpenAI adapter yields nothing, which would report a comfortable zero.
func scanCoverageAdapterFor(name string) (traffic.Adapter, string) {
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
		return &trafficopenai.Adapter{}, "/v1/chat/completions"
	}
}

// forEachCorpusFrame walks every SSE frame of every captured stream.
func forEachCorpusFrame(t *testing.T, fn func(corpus, payload string, adapter traffic.Adapter, path string)) {
	t.Helper()
	dir := scanCoverageCorpusDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the streaming corpora at %s: %v", dir, err)
	}
	eventSplit := regexp.MustCompile(`\n\s*\n`)
	seen := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".response.sse") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".response.sse")
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		adapter, path := scanCoverageAdapterFor(name)
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
			seen++
			fn(name, strings.Join(data, "\n"), adapter, path)
		}
	}
	if seen == 0 {
		t.Fatal("walked zero frames — the corpus directory or the SSE split changed shape, and a " +
			"walk that visits nothing passes every assertion below")
	}
}

// TestEveryStreamingCorpusYieldsScannableText is the coverage floor. A wire that
// extracts nothing is a wire delivered unscanned; before the Responses event
// grammar was added, a captured 206-frame stream carrying 674 bytes of assistant
// and reasoning text extracted zero, and every test in the tree stayed green.
func TestEveryStreamingCorpusYieldsScannableText(t *testing.T) {
	perCorpus := map[string]int{}
	forEachCorpusFrame(t, func(corpus, payload string, adapter traffic.Adapter, path string) {
		codec := adapterWireCodec{ctx: context.Background(), adapter: adapter, path: path, scratch: new([]byte)}
		if txt, ok := codec.ChunkText(payload); ok {
			perCorpus[corpus] += len(txt)
		}
	})

	for corpus, n := range perCorpus {
		t.Logf("%-32s scannable text=%d bytes", corpus, n)
	}
	// Tool-only streams legitimately carry no scannable text; name them rather
	// than weakening the floor for every corpus.
	toolOnly := map[string]bool{
		"anthropic_thinking_tools": true,
		"gemini_tools":             true,
		"moonshot_tools":           true,
		"openai_tools":             true,
		"openai_responses_tools":   true,
	}
	forEachCorpusName(t, func(corpus string) {
		if toolOnly[corpus] {
			return
		}
		if perCorpus[corpus] == 0 {
			t.Errorf("%s extracted ZERO scannable text. On the compliance substrate that is a "+
				"stream delivered to the client with nothing scanned, and it looks identical to "+
				"a stream that carried no text", corpus)
		}
	})
}

// TestReasoningReachesTheScanBuffer pins the channel the substrate used to drop.
// ReasoningSegments is where every adapter puts chain-of-thought — Anthropic
// thinking_delta, Gemini thought=true, OpenAI/DeepSeek reasoning_content, Cohere
// tool_plan — and ChunkText joined only Segments, so the model's thinking reached
// the client having been scanned by nothing.
func TestReasoningReachesTheScanBuffer(t *testing.T) {
	reasoningFrames, reachedBuffer := 0, 0
	forEachCorpusFrame(t, func(corpus, payload string, adapter traffic.Adapter, path string) {
		nc, err := adapter.ExtractStreamChunk(context.Background(), []byte(payload), path)
		if err != nil {
			return
		}
		r := strings.Join(nc.ReasoningSegments, "")
		if r == "" {
			return
		}
		reasoningFrames++
		codec := adapterWireCodec{ctx: context.Background(), adapter: adapter, path: path, scratch: new([]byte)}
		txt, ok := codec.ChunkText(payload)
		if ok && strings.Contains(txt, r) {
			reachedBuffer++
		}
	})

	if reasoningFrames == 0 {
		t.Fatal("no corpus frame carries reasoning — this test cannot observe the channel it " +
			"guards, so a regression that drops reasoning again would pass it")
	}
	if reachedBuffer != reasoningFrames {
		t.Errorf("%d of %d reasoning-bearing frames reached the scan buffer; the rest are "+
			"chain-of-thought delivered to the client and scanned by nothing",
			reachedBuffer, reasoningFrames)
	}
	t.Logf("reasoning-bearing frames: %d, all reaching the scan buffer", reasoningFrames)
}

// TestSSEFrameChannelsAreSpliceable pins the assumption ChunkText's join rests
// on: the text it reports is one JSON value inside the frame, which is what
// reencodeChunkText replaces. Joining content and reasoning is safe only while no
// frame carries both — measured true across every captured stream. A provider
// that starts interleaving them makes this red, which is the signal to give the
// splice a per-channel path rather than to discover it as a redaction that
// silently could not be applied.
func TestSSEFrameChannelsAreSpliceable(t *testing.T) {
	textFrames, spliceable, bothChannels := 0, 0, 0
	forEachCorpusFrame(t, func(corpus, payload string, adapter traffic.Adapter, path string) {
		nc, err := adapter.ExtractStreamChunk(context.Background(), []byte(payload), path)
		if err != nil {
			return
		}
		c := strings.Join(nc.Segments, "")
		r := strings.Join(nc.ReasoningSegments, "")
		if c != "" && r != "" {
			bothChannels++
		}
		codec := adapterWireCodec{ctx: context.Background(), adapter: adapter, path: path, scratch: new([]byte)}
		txt, ok := codec.ChunkText(payload)
		if !ok {
			return
		}
		textFrames++
		enc, merr := json.Marshal(txt)
		if merr == nil && strings.Contains(payload, string(enc)) {
			spliceable++
		}
	})

	if textFrames == 0 {
		t.Fatal("no frame reported text — the assertion below would hold vacuously")
	}
	if bothChannels != 0 {
		t.Errorf("%d frames carry BOTH content and reasoning. ChunkText joins the two, and a "+
			"joined string spanning two separate JSON values cannot be spliced — those "+
			"redactions would fail closed on the proxy and fail open on the agent. Give the "+
			"splice a per-channel path", bothChannels)
	}
	if spliceable != textFrames {
		t.Errorf("%d of %d text-bearing frames are spliceable; on the rest a decided redaction "+
			"cannot be written back", spliceable, textFrames)
	}
	t.Logf("text-bearing frames: %d, all spliceable; frames carrying both channels: %d",
		textFrames, bothChannels)
}

// forEachCorpusName enumerates the corpora by name, so a floor can be asserted
// per corpus rather than only over the total.
func forEachCorpusName(t *testing.T, fn func(corpus string)) {
	t.Helper()
	entries, err := os.ReadDir(scanCoverageCorpusDir())
	if err != nil {
		t.Fatalf("read corpora: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".response.sse") {
			fn(strings.TrimSuffix(e.Name(), ".response.sse"))
		}
	}
}
