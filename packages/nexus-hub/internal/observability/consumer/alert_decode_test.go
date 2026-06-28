package consumer

import (
	"encoding/binary"
	"testing"
	"time"

	json "github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// binAlertFrame builds a binary audit frame (magic byte, then length-prefixed
// records) the way the gateway batches records, so DecodeAlertFrame's binary path
// can be exercised end-to-end.
func binAlertFrame(msgs ...mq.TrafficEventMessage) []byte {
	frame := []byte{mq.BinwireMagic}
	for _, m := range msgs {
		rec := m.AppendBinary(nil)
		frame = binary.AppendUvarint(frame, uint64(len(rec)))
		frame = append(frame, rec...)
	}
	return frame
}

func ndjsonAlertFrame(t *testing.T, msgs ...mq.TrafficEventMessage) []byte {
	t.Helper()
	var out []byte
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, b...)
	}
	return out
}

// TestDecodeAlertFrame_SkipBodyAndFields proves the alert-path decode lands every
// field an aggregator reads AND skips the inline body content (zero-copy): body
// metadata (Kind/Truncated) is present, but InlineBytes is nil — the 256 KB-class
// payload the alert rules never read is never copied.
func TestDecodeAlertFrame_SkipBodyAndFields(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	ec := "RATE_LIMITED"
	msg := mq.TrafficEventMessage{
		ID:            "evt-1",
		Source:        "ai-gateway",
		SourceProcess: "chat",
		Timestamp:     ts,
		LatencyMs:     42,
		StatusCode:    429,
		ErrorCode:     &ec,
		EntityID:      "vk-7",
		EntityType:    "virtual_key",
		ModelID:       "gpt-4o",
		TotalTokens:   123,
		RequestBody:   audit.NewInlineBody([]byte(`{"messages":[{"role":"user","content":"secret prompt"}]}`), 55, false, "application/json"),
		ResponseBody:  audit.NewInlineBody([]byte("event: delta\ndata: hi\n\n"), 23, true, "text/event-stream"),
	}

	var got *AlertView
	var captured AlertView
	DecodeAlertFrame(binAlertFrame(msg), func(e *AlertView) {
		captured = *e // copy out (pointer is pooled/transient)
		got = &captured
	}, func(err error) { t.Fatalf("unexpected decode error: %v", err) })

	if got == nil {
		t.Fatal("onRecord never fired")
	}
	if got.LatencyMs == nil || *got.LatencyMs != 42 {
		t.Errorf("LatencyMs = %v, want 42", got.LatencyMs)
	}
	if got.StatusCode == nil || *got.StatusCode != 429 {
		t.Errorf("StatusCode = %v, want 429", got.StatusCode)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "RATE_LIMITED" {
		t.Errorf("ErrorCode = %v, want RATE_LIMITED", got.ErrorCode)
	}
	if got.EntityID == nil || *got.EntityID != "vk-7" {
		t.Errorf("EntityID = %v, want vk-7", got.EntityID)
	}
	if got.ModelID == nil || *got.ModelID != "gpt-4o" {
		t.Errorf("ModelID = %v, want gpt-4o", got.ModelID)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 123 {
		t.Errorf("TotalTokens = %v, want 123", got.TotalTokens)
	}
	// Body metadata present, content skipped.
	if got.RequestBody.Kind != audit.BodyInline || got.RequestBody.InlineBytes != nil {
		t.Errorf("RequestBody: kind=%q inlineBytes=%d — want inline, zero-copy (nil bytes)", got.RequestBody.Kind, len(got.RequestBody.InlineBytes))
	}
	if got.ResponseBody.Kind != audit.BodyInline || !got.ResponseBody.Truncated || got.ResponseBody.InlineBytes != nil {
		t.Errorf("ResponseBody: kind=%q truncated=%v inlineBytes=%d — want inline+truncated, no content", got.ResponseBody.Kind, got.ResponseBody.Truncated, len(got.ResponseBody.InlineBytes))
	}
}

// TestDecodeAlertFrame_MultiRecordAndJSON covers batched binary frames and the
// legacy NDJSON path, and proves the pooled decode target is reset between records
// (record 2's absent ErrorCode must not inherit record 1's value).
func TestDecodeAlertFrame_MultiRecordAndJSON(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	ec := "UPSTREAM_5XX"
	// entityId is the per-record marker (AlertView omits the producer's id).
	m1 := mq.TrafficEventMessage{ID: "a", EntityID: "a", Source: "ai-gateway", Timestamp: ts, LatencyMs: 1, ErrorCode: &ec}
	m2 := mq.TrafficEventMessage{ID: "b", EntityID: "b", Source: "ai-gateway", Timestamp: ts, LatencyMs: 2} // no ErrorCode

	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"binary_batch", binAlertFrame(m1, m2)},
		{"ndjson_batch", ndjsonAlertFrame(t, m1, m2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type rec struct {
				id    string
				hasEC bool
			}
			var recs []rec
			DecodeAlertFrame(tc.frame, func(e *AlertView) {
				id := ""
				if e.EntityID != nil {
					id = *e.EntityID
				}
				recs = append(recs, rec{id: id, hasEC: e.ErrorCode != nil})
			}, func(err error) { t.Fatalf("decode error: %v", err) })

			if len(recs) != 2 {
				t.Fatalf("decoded %d records, want 2", len(recs))
			}
			if recs[0].id != "a" || recs[1].id != "b" {
				t.Errorf("ids = %q,%q want a,b", recs[0].id, recs[1].id)
			}
			if !recs[0].hasEC {
				t.Error("record a must carry ErrorCode")
			}
			if recs[1].hasEC {
				t.Error("record b ErrorCode leaked from record a — pool not reset")
			}
		})
	}
}

// BenchmarkAlertDecodeVsFull quantifies the alert-path win: the full DB-writer
// decode copies the inline body + allocates a fresh TrafficEventMessage per
// record; DecodeAlertFrame skips the body content (zero-copy) and reuses a pooled
// target. Compare ns/op + B/op + allocs/op. The body is a realistic ~16 KB SSE
// response so the copy the alert path avoids is visible.
func benchRecord() []byte {
	audit.SetInlineCompression(false, 1<<20, 3) // keep body raw so the copy is the full size
	body := make([]byte, 16*1024)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	ec := "OK"
	m := mq.TrafficEventMessage{
		ID: "evt-bench", Source: "ai-gateway", SourceProcess: "chat",
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		LatencyMs: 42, StatusCode: 200, ErrorCode: &ec, EntityID: "vk-7", EntityType: "vk",
		ModelID: "gpt-4o", ProviderID: "openai", TotalTokens: 1234,
		RequestBody:  audit.NewInlineBody(body, int64(len(body)), false, "application/json"),
		ResponseBody: audit.NewInlineBody(body, int64(len(body)), false, "text/event-stream"),
	}
	return m.AppendBinary(nil)
}

func BenchmarkFullRecordDecode(b *testing.B) {
	rec := benchRecord()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decodeBinaryRecord(rec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAlertRecordDecode(b *testing.B) {
	frame := append([]byte{mq.BinwireMagic}, appendUvarintRec(benchRecord())...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DecodeAlertFrame(frame, func(*AlertView) {}, func(error) { b.Fatal("decode error") })
	}
}

func appendUvarintRec(rec []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(len(rec)))
	return append(out, rec...)
}

// TestDecodeAlertFrame_CorruptRecordReportsError confirms a malformed binary
// record is reported via onError (poison-skipped), never a panic.
func TestDecodeAlertFrame_CorruptRecordReportsError(t *testing.T) {
	// magic + length-prefixed record whose bytes are an unknown/short field id run.
	frame := []byte{mq.BinwireMagic}
	bad := []byte{0x80} // bad field-id (continuation byte with no follow → uvarint error)
	frame = binary.AppendUvarint(frame, uint64(len(bad)))
	frame = append(frame, bad...)

	var errs, ok int
	DecodeAlertFrame(frame, func(*AlertView) { ok++ }, func(error) { errs++ })
	if errs == 0 {
		t.Error("corrupt record must surface via onError")
	}
}
