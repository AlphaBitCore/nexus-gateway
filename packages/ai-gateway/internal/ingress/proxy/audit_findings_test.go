// Unit tests for the ingress/proxy audit-finding fixes: the embeddings
// metadata helpers (base64 dimension, token-id batch sizing),
// the quota downgrade-budget + cost-limit helpers, the
// unpriced-cost metadata stamp, and the SSE reader terminal-error
// classification. Each test asserts the named failure mode, not
// just line execution.
package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// --- embeddingBatchSize ---

func TestEmbeddingBatchSize(t *testing.T) {
	cases := []struct {
		name string
		json string // value of the "input" field
		want int
	}{
		{"single string", `"hello"`, 1},
		{"array of strings", `["a","b","c"]`, 3},
		{"token-id sequence is one input", `[1,2,3]`, 1},
		{"batch of token-id sequences", `[[1,2],[3,4]]`, 2},
		{"empty array", `[]`, 1},
		{"single number", `5`, 1},
		{"object (not array/string)", `{"x":1}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gjson.Parse(tc.json)
			if got := embeddingBatchSize(in); got != tc.want {
				t.Fatalf("embeddingBatchSize(%s)=%d want %d", tc.json, got, tc.want)
			}
		})
	}
}

// --- embeddingResponseDimension (incl. base64 string vectors) ---

func TestEmbeddingResponseDimension(t *testing.T) {
	// 3-element float32 vector packed as base64 → 12 bytes → /4 = 3.
	stdB64 := base64.StdEncoding.EncodeToString(make([]byte, 12))
	// 2-element vector encoded UNPADDED so the StdEncoding decode fails and
	// the RawStdEncoding fallback is exercised (8 bytes is not a multiple of
	// 3, so its standard encoding carries padding the raw form omits).
	rawB64 := base64.RawStdEncoding.EncodeToString(make([]byte, 8))

	cases := []struct {
		name string
		body string
		want int
	}{
		{"float array", `{"data":[{"embedding":[0.1,0.2,0.3,0.4]}]}`, 4},
		{"base64 string vector", fmt.Sprintf(`{"data":[{"embedding":%q}]}`, stdB64), 3},
		{"unpadded base64 vector", fmt.Sprintf(`{"data":[{"embedding":%q}]}`, rawB64), 2},
		{"empty data array", `{"data":[]}`, 0},
		{"missing embedding", `{"data":[{}]}`, 0},
		{"empty base64 string", `{"data":[{"embedding":""}]}`, 0},
		{"undecodable base64", `{"data":[{"embedding":"!!!notbase64"}]}`, 0},
		{"too-short base64 (<4 bytes)", fmt.Sprintf(`{"data":[{"embedding":%q}]}`, base64.StdEncoding.EncodeToString(make([]byte, 2))), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := embeddingResponseDimension([]byte(tc.body)); got != tc.want {
				t.Fatalf("embeddingResponseDimension(%s)=%d want %d", tc.body, got, tc.want)
			}
		})
	}
}

// TestUpdateEmbeddingDimension_Base64NoFalseWarning is the named
// failure mode: a valid base64 embedding response must NOT be stamped with
// warning="empty_data_array", and must carry the decoded dimension.
func TestUpdateEmbeddingDimension_Base64NoFalseWarning(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(make([]byte, 16)) // 4 floats
	body := fmt.Sprintf(`{"data":[{"embedding":%q}]}`, b64)

	md := updateEmbeddingDimension(nil, []byte(body))
	m, ok := md.(map[string]any)
	if !ok {
		t.Fatalf("metadata not a map: %T", md)
	}
	emb, _ := m["embedding"].(map[string]any)
	if emb == nil {
		t.Fatal("no embedding submap")
	}
	if _, hasWarn := emb["warning"]; hasWarn {
		t.Errorf("base64 vector falsely warned: %v", emb["warning"])
	}
	if emb["dimension"] != 4 {
		t.Errorf("dimension=%v want 4", emb["dimension"])
	}
}

func TestUpdateEmbeddingDimension_GenuinelyEmptyWarns(t *testing.T) {
	md := updateEmbeddingDimension(nil, []byte(`{"data":[]}`))
	emb := md.(map[string]any)["embedding"].(map[string]any)
	if emb["warning"] != "empty_data_array" {
		t.Errorf("empty data should warn; got %v", emb["warning"])
	}
}

// --- quota helpers ---

func TestQuotaHasCostLimit(t *testing.T) {
	if quotaHasCostLimit(nil) {
		t.Error("nil decision must report no cost limit")
	}
	if quotaHasCostLimit(&quota.Decision{Levels: []quota.CheckLevel{{TargetType: "virtual_key"}}}) {
		t.Error("level without HasLimit must report no cost limit")
	}
	d := &quota.Decision{Levels: []quota.CheckLevel{
		{TargetType: "virtual_key"},
		{TargetType: "organization", HasLimit: true, LimitCents: 100},
	}}
	if !quotaHasCostLimit(d) {
		t.Error("a level with HasLimit must report a cost limit")
	}
}

func TestQuotaDowngradeBudget(t *testing.T) {
	if got := quotaDowngradeBudget(nil); got != 0 {
		t.Errorf("nil decision budget=%v want 0", got)
	}
	// No enforced level → 0.
	if got := quotaDowngradeBudget(&quota.Decision{Levels: []quota.CheckLevel{{TargetType: "vk"}}}); got != 0 {
		t.Errorf("no-limit budget=%v want 0", got)
	}
	// Budget is the MINIMUM remaining headroom across enforced levels:
	// vk has $4 left (500-100), org has $1.50 left (300-150) → $1.50.
	d := &quota.Decision{Levels: []quota.CheckLevel{
		{TargetType: "vk", HasLimit: true, LimitCents: 500, CurrentCents: 100},
		{TargetType: "org", HasLimit: true, LimitCents: 300, CurrentCents: 150},
		{TargetType: "user"}, // no limit — ignored
	}}
	if got := quotaDowngradeBudget(d); got != 1.5 {
		t.Errorf("min-headroom budget=%v want 1.5", got)
	}
	// A level already over its cap contributes 0 (clamped, not negative).
	over := &quota.Decision{Levels: []quota.CheckLevel{
		{TargetType: "vk", HasLimit: true, LimitCents: 100, CurrentCents: 500},
	}}
	if got := quotaDowngradeBudget(over); got != 0 {
		t.Errorf("over-cap budget=%v want 0 (clamped)", got)
	}
}

// --- stampUnpricedCost ---

func TestStampUnpricedCost(t *testing.T) {
	md := stampUnpricedCost(nil)
	cost := md.(map[string]any)["cost"].(map[string]any)
	if cost["unpriced"] != true {
		t.Fatalf("unpriced flag not set: %v", cost)
	}
	// Preserves existing metadata keys.
	md2 := stampUnpricedCost(map[string]any{"embedding": map[string]any{"dimension": 3}})
	m := md2.(map[string]any)
	if m["embedding"].(map[string]any)["dimension"] != 3 {
		t.Error("stampUnpricedCost dropped existing metadata")
	}
	if m["cost"].(map[string]any)["unpriced"] != true {
		t.Error("stampUnpricedCost did not set unpriced on existing map")
	}
}

// --- chunkSSEReader terminal-error classification ---

func TestChunkSSEReader_TerminalError_UpstreamError(t *testing.T) {
	sub := &queuedChunkSub{entries: []queuedChunkEntry{{err: errors.New("boom")}}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI, false)
	rd.usageSink = &chunkUsageHolder{}

	// Drain until EOF so the terminal error is published.
	buf := make([]byte, 4096)
	for {
		if _, err := rd.Read(buf); err != nil {
			break
		}
	}
	term := rd.terminalError()
	if term == nil {
		t.Fatal("expected a terminal error after upstream stream fault")
	}
	if term.code != streamErrCodeUpstream {
		t.Errorf("code=%q want %q", term.code, streamErrCodeUpstream)
	}
}

func TestChunkSSEReader_TerminalError_ClientAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client gone
	sub := &queuedChunkSub{entries: []queuedChunkEntry{{err: context.Canceled}}}
	rd := newChunkSSEReaderFromSubscription(ctx, sub, nil, provcore.FormatOpenAI, false)
	rd.usageSink = &chunkUsageHolder{}

	if _, err := rd.Read(make([]byte, 64)); err == nil {
		t.Fatal("expected the cancel error to propagate")
	}
	term := rd.terminalError()
	if term == nil || term.code != streamErrCodeClientAbort {
		t.Fatalf("expected CLIENT_ABORT terminal error; got %+v", term)
	}
}

func TestChunkSSEReader_TerminalError_CleanEOF(t *testing.T) {
	sub := &queuedChunkSub{entries: []queuedChunkEntry{
		{chunk: provcore.Chunk{Done: true, RawBytes: []byte("data: [DONE]\n\n")}},
	}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI, false)
	rd.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 64)
	for {
		if _, err := rd.Read(buf); err != nil {
			break
		}
	}
	if term := rd.terminalError(); term != nil {
		t.Errorf("clean EOF must not set a terminal error; got %+v", term)
	}
}

// errTranscoder fails on the first non-Done chunk, exercising the
// cross-format transcoder terminal-error branch.
type errTranscoder struct{}

func (errTranscoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	if chunk.Done {
		return nil, nil
	}
	return nil, errors.New("transcode failure")
}

func TestChunkSSEReader_TerminalError_TranscoderError(t *testing.T) {
	sub := &queuedChunkSub{entries: []queuedChunkEntry{{chunk: provcore.Chunk{Delta: "x"}}}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, errTranscoder{}, provcore.FormatOpenAI, false)
	rd.usageSink = &chunkUsageHolder{}

	if _, err := rd.Read(make([]byte, 64)); err == nil {
		t.Fatal("expected the transcoder error to propagate")
	}
	term := rd.terminalError()
	if term == nil || term.code != streamErrCodeUpstream {
		t.Fatalf("expected UPSTREAM_STREAM_ERROR from transcoder fault; got %+v", term)
	}
	_ = io.EOF // keep io imported for symmetry with sibling tests
}
