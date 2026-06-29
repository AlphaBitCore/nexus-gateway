package consumer

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestSplitBinaryFrameMultiRecord validates the frame contract shared by the gw
// publisher (writer_batch.publishFramed) and the Hub: a binary frame is the magic
// byte followed by length-prefixed records. It builds a 3-record frame the same
// way the publisher does, splits it, decodes each record, and checks identity +
// count — so a drift in the magic/length framing between the two sides fails here.
func TestSplitBinaryFrameMultiRecord(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	msgs := []mq.TrafficEventMessage{
		{ID: "a", Source: "ai-gateway", Timestamp: ts, LatencyMs: 1, ModelID: "m1"},
		{ID: "b", Source: "ai-gateway", Timestamp: ts, LatencyMs: 2, PromptTokens: 99},
		{ID: "c", Source: "agent", Timestamp: ts, LatencyMs: 3},
	}

	// Build the frame exactly as publishFramed does: [magic][uvarint len][rec]…
	frame := []byte{mq.BinwireMagic}
	for i := range msgs {
		rec := msgs[i].AppendBinary(nil)
		frame = binary.AppendUvarint(frame, uint64(len(rec)))
		frame = append(frame, rec...)
	}

	if !isBinaryFrame(frame) {
		t.Fatal("isBinaryFrame=false for a magic-prefixed frame")
	}
	recs := splitBinaryFrame(frame)
	if len(recs) != len(msgs) {
		t.Fatalf("split record count = %d, want %d", len(recs), len(msgs))
	}
	for i, rec := range recs {
		got, err := decodeBinaryRecord(rec)
		if err != nil {
			t.Fatalf("record %d decode: %v", i, err)
		}
		if got.ID != msgs[i].ID {
			t.Fatalf("record %d ID = %q, want %q", i, got.ID, msgs[i].ID)
		}
		if got.Source != msgs[i].Source {
			t.Fatalf("record %d Source = %q, want %q", i, got.Source, msgs[i].Source)
		}
		if got.LatencyMs == nil || *got.LatencyMs != msgs[i].LatencyMs {
			t.Fatalf("record %d LatencyMs mismatch", i)
		}
	}

	// A non-magic frame is NOT treated as binary (dual-read falls to JSON).
	if isBinaryFrame([]byte(`{"id":"x"}`)) {
		t.Fatal("JSON frame misdetected as binary")
	}
	// A truncated frame yields the records parsed so far, never a panic.
	if got := splitBinaryFrame(frame[:len(frame)-3]); len(got) != len(msgs)-1 {
		t.Fatalf("truncated frame split = %d records, want %d", len(got), len(msgs)-1)
	}
}
