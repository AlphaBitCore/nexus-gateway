package consumer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestBinwirePooledFrameRoundTrip covers the pooled-buffer encode path: records are
// marshalled into a POOLED bytes.Buffer via AvailableBuffer() (the record lives in a
// 64 KB backing with large spare capacity, where Body.appendLenPrefixed does its
// length-prefix shift IN PLACE — a shift that must not corrupt neighbouring records),
// with realistic ~50 KB s2-compressible bodies, marshalled CONCURRENTLY, then framed
// like writer_batch.publishFramed (magic + uvarint-len records) and split+decoded by
// the Hub path. Every record must decode in order with no desync.
func TestBinwirePooledFrameRoundTrip(t *testing.T) {
	audit.SetInlineCompression(true, 1024, 3)
	const n = 300
	msgs := make([]mq.TrafficEventMessage, n)
	for i := range msgs {
		msgs[i] = realisticMsg(i)
	}

	// Marshal concurrently into pooled buffers — mirrors marshalChunkParallel +
	// marshalRecordBinary (buf.Reset(); AppendBinary(buf.AvailableBuffer())).
	pool := sync.Pool{New: func() any { b := new(bytes.Buffer); b.Grow(64 << 10); return b }}
	datas := make([][]byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := pool.Get().(*bytes.Buffer)
			buf.Reset()
			datas[i] = msgs[i].AppendBinary(buf.AvailableBuffer())
			// NB: buf intentionally NOT returned to the pool until after framing in
			// prod (reclaimMsgBufs); keep it referenced so the backing stays put.
			_ = buf
		}(i)
	}
	wg.Wait()

	// Frame like publishFramed: pack into 262144-byte binary frames.
	frames := buildBinaryFramesForTest(datas, 262144)

	// Split + decode every record across all frames, in order.
	var got []TrafficEventMessage
	for fi, fr := range frames {
		if !isBinaryFrame(fr) {
			t.Fatalf("frame %d not detected as binary", fi)
		}
		for _, rec := range splitBinaryFrame(fr) {
			d, err := decodeBinaryRecord(rec)
			if err != nil {
				t.Fatalf("frame %d: decode error %v (record %d bytes, first=%x)", fi, err, len(rec), rec[:min(8, len(rec))])
			}
			got = append(got, d)
		}
	}
	if len(got) != n {
		t.Fatalf("record count: got %d want %d (desync mis-split the frame)", len(got), n)
	}
	for i := range got {
		if got[i].ID != msgs[i].ID {
			t.Fatalf("record %d ID=%q want %q (frame desync)", i, got[i].ID, msgs[i].ID)
		}
		if got[i].RequestBody.Kind != msgs[i].RequestBody.Kind {
			t.Fatalf("record %d req body kind=%q want %q", i, got[i].RequestBody.Kind, msgs[i].RequestBody.Kind)
		}
	}
}

// buildBinaryFramesForTest mirrors writer_batch.publishFramed's binary framing.
func buildBinaryFramesForTest(datas [][]byte, frameMax int) [][]byte {
	var frames [][]byte
	for start := 0; start < len(datas); {
		end := start
		size := 1 // magic
		for end < len(datas) {
			need := uvarintLenForTest(uint64(len(datas[end]))) + len(datas[end])
			if end > start && size+need > frameMax {
				break
			}
			size += need
			end++
		}
		frame := make([]byte, 0, size)
		frame = append(frame, mq.BinwireMagic)
		for i := start; i < end; i++ {
			frame = binary.AppendUvarint(frame, uint64(len(datas[i])))
			frame = append(frame, datas[i]...)
		}
		frames = append(frames, frame)
		start = end
	}
	return frames
}

func uvarintLenForTest(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// realisticMsg builds a message shaped like real gateway traffic: a ~50 KB
// JSON-ish (s2-compressible) request body forced to EncodingS2, a small response
// body, and varied scalar/pointer fields — so the record is ~10-20 KB and lives
// inside the pooled 64 KB buffer's spare capacity (the untested in-place path).
func realisticMsg(i int) mq.TrafficEventMessage {
	// Build a ~50 KB JSON body with enough variety that s2 still produces a
	// multi-KB frame (not a trivial one), exercising appendLenPrefixed.
	var sb bytes.Buffer
	sb.WriteString(`{"model":"mock-gpt-4o","messages":[`)
	for j := 0; j < 400; j++ {
		fmt.Fprintf(&sb, `{"role":"user","content":"message %d-%d lorem ipsum dolor sit amet consectetur %d"},`, i, j, i*j)
	}
	sb.WriteString(`{"role":"user","content":"end"}],"max_tokens":256}`)
	reqBody := audit.Body{Kind: audit.BodyInline, Encoding: audit.EncodingS2, InlineBytes: sb.Bytes(), SizeBytes: int64(sb.Len()), ContentType: "application/json"}
	respBody := audit.NewInlineBody([]byte(`{"choices":[{"message":{"content":"hi there"}}],"usage":{"total_tokens":17}}`), 74, false, "application/json")
	sp := func(s string) *string { return &s }
	return mq.TrafficEventMessage{
		ID:               fmt.Sprintf("evt-%06d", i),
		Source:           "ai-gateway",
		Timestamp:        time.Unix(1_700_000_000+int64(i), 0).UTC(),
		LatencyMs:        i % 500,
		Method:           "POST",
		Path:             "/v1/chat/completions",
		StatusCode:       200,
		EndpointType:     "chat",
		IngressFormat:    "openai",
		ModelID:          "gpt-4o",
		RoutedModelID:    "a5f8d68b-ab40-45a7-96ca-c2141548440b",
		PromptTokens:     int64(1000 + i),
		CompletionTokens: 256,
		TotalTokens:      int64(1256 + i),
		EstimatedCostUsd: 0.0001 * float64(i),
		CacheStatus:      "MISS",
		Identity:         map[string]any{"sub": fmt.Sprintf("user-%d", i), "tier": "pro"},
		CredentialID:     "cred-1",
		OriginTZ:         sp("America/New_York"),
		RequestBody:      reqBody,
		ResponseBody:     respBody,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
