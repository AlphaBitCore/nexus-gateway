package consumer

import (
	"encoding/binary"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestSplitBinaryFrameEdgeCases covers the two early-out branches splitBinaryFrame
// takes on inputs that are not a well-formed magic+length stream: a non-magic /
// empty buffer returns nil (the dual-read fall-through to JSON), and a frame whose
// record length-prefix is a truncated uvarint stops at the records parsed so far
// rather than panicking.
func TestSplitBinaryFrameEdgeCases(t *testing.T) {
	// Empty input → nil (the len(data) < 1 guard).
	if got := splitBinaryFrame(nil); got != nil {
		t.Fatalf("empty input: got %v records, want nil", got)
	}
	// Non-magic first byte → nil (dual-read routes this to the JSON path).
	if got := splitBinaryFrame([]byte("{\"id\":\"x\"}")); got != nil {
		t.Fatalf("JSON input: got %v records, want nil", got)
	}

	// One good record, then a truncated length prefix (bare 0x80 continuation).
	src := mq.TrafficEventMessage{ID: "a", Source: "ai-gateway", LatencyMs: 1}
	rec := src.AppendBinary(nil)
	frame := []byte{mq.BinwireMagic}
	frame = binary.AppendUvarint(frame, uint64(len(rec)))
	frame = append(frame, rec...)
	frame = append(frame, 0x80) // malformed length prefix → break at line 135

	got := splitBinaryFrame(frame)
	if len(got) != 1 {
		t.Fatalf("malformed-length frame: got %d records, want 1 (the good one)", len(got))
	}
	dec, err := decodeBinaryRecord(got[0])
	if err != nil {
		t.Fatalf("decode first record: %v", err)
	}
	if dec.ID != "a" {
		t.Fatalf("first record ID = %q, want \"a\"", dec.ID)
	}
}

// TestSplitBinaryFrameLengthOverflowNoPanic feeds a record length prefix >= 2^63
// (where int(ln) goes negative and would wrap the bounds check into a slice panic)
// and asserts splitBinaryFrame truncates cleanly — returning the records parsed so
// far — instead of crashing the Hub. One bad frame must never take the process down.
func TestSplitBinaryFrameLengthOverflowNoPanic(t *testing.T) {
	src := mq.TrafficEventMessage{ID: "a", Source: "ai-gateway", LatencyMs: 1}
	rec := src.AppendBinary(nil)
	frame := []byte{mq.BinwireMagic}
	frame = binary.AppendUvarint(frame, uint64(len(rec)))
	frame = append(frame, rec...)
	// Second record claims ~18 EiB (high bit set → negative as int).
	frame = binary.AppendUvarint(frame, ^uint64(0))
	frame = append(frame, 'x', 'y')

	got := splitBinaryFrame(frame)
	if len(got) != 1 {
		t.Fatalf("overflow-length frame: got %d records, want 1 (the good one before the lie)", len(got))
	}
	if dec, err := decodeBinaryRecord(got[0]); err != nil || dec.ID != "a" {
		t.Fatalf("first record decode: id=%q err=%v, want id=\"a\" err=nil", dec.ID, err)
	}
}

// TestPayloadBytesFallsBackToMsgData covers the unframed fallback in
// pendingTrafficMessage.payloadBytes: when the per-record raw slice is nil (a
// directly-constructed, non-framed record), the DLQ payload is the whole shared
// NATS message data.
func TestPayloadBytesFallsBackToMsgData(t *testing.T) {
	want := []byte(`{"id":"legacy"}`)
	pm := pendingTrafficMessage{msg: &mq.Message{Data: want}}
	if got := pm.payloadBytes(); string(got) != string(want) {
		t.Fatalf("unframed payloadBytes = %q, want %q (msg.Data fallback)", got, want)
	}

	// And when raw IS set, it wins over msg.Data (the per-record frame line).
	raw := []byte("record-line")
	pm2 := pendingTrafficMessage{raw: raw, msg: &mq.Message{Data: want}}
	if got := pm2.payloadBytes(); string(got) != string(raw) {
		t.Fatalf("framed payloadBytes = %q, want %q (raw line)", got, raw)
	}
}
