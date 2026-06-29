package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// withCompression enables inline compression for the duration of a test and
// restores the disabled default afterwards, so the process-global toggle never
// leaks across tests.
func withCompression(t *testing.T, minBytes, level int) {
	t.Helper()
	SetInlineCompression(true, minBytes, level)
	t.Cleanup(func() { SetInlineCompression(false, 0, 0) })
}

// roundTripBody simulates the full pipeline a captured body travels:
//
//	producer NewInlineBody → MarshalJSON (wire) → UnmarshalJSON (hub) →
//	ColumnPayload (persist) → DecodeBodyForColumn (CP view)
//
// and returns the recovered bytes plus the wire inlineBytes string and the
// persisted column (payload,encoding) so a test can assert both fidelity and
// the pass-through invariant.
func roundTripBody(t *testing.T, original []byte) (recovered []byte, wireInline string, colPayload []byte, colEnc string) {
	t.Helper()
	producer := NewInlineBody(original, int64(len(original)), false, "application/json")

	wire, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var hub Body
	if err := json.Unmarshal(wire, &hub); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}

	colPayload, colEnc = hub.ColumnPayload()
	recovered = DecodeBodyForColumn(colPayload, colEnc)

	// Pull the raw inlineBytes string off the wire for the pass-through assertion.
	var probe struct {
		InlineBytes json.RawMessage `json:"inlineBytes"`
	}
	if err := json.Unmarshal(wire, &probe); err != nil {
		t.Fatalf("probe wire: %v", err)
	}
	var s string
	// inlineBytes is a JSON string for text/base64/zstd; for raw it is a JSON
	// value. Only the string forms matter for the pass-through assertion.
	if json.Unmarshal(probe.InlineBytes, &s) == nil {
		wireInline = s
	}
	return recovered, wireInline, colPayload, colEnc
}

func TestInlineCompression_RoundTripPreservesBytes(t *testing.T) {
	withCompression(t, 16, 0) // low floor so the small test bodies compress

	cases := []struct {
		name string
		body []byte
	}{
		{"json", []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("hello world ", 200) + `"}]}`)},
		{"text_sse", []byte("data: " + strings.Repeat("token ", 500) + "\n\ndata: [DONE]\n\n")},
		{"binary_with_nul", append([]byte{0x00, 0x1b, 0xff, 0xfe}, bytes.Repeat([]byte{0x00, 0x01, 0x02}, 400)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recovered, wireInline, colPayload, colEnc := roundTripBody(t, tc.body)
			if !bytes.Equal(recovered, tc.body) {
				t.Fatalf("recovered bytes differ: got %d bytes, want %d", len(recovered), len(tc.body))
			}
			if colEnc != BodyColumnS2 {
				t.Fatalf("column encoding = %q, want %q", colEnc, BodyColumnS2)
			}
			// Persist invariant (BYTEA): the column holds the RAW frame, and the wire
			// inlineBytes is base64 of that exact frame — i.e. the Hub base64-DECODED
			// the wire form to the column (no decompress, no re-compress), which is
			// what removes the +33% base64 inflation from the disk write.
			if got := base64.StdEncoding.EncodeToString(colPayload); got != wireInline {
				t.Fatalf("base64(column) != wire inlineBytes (hub did not base64-decode cleanly): col=%d wire=%d bytes", len(colPayload), len(wireInline))
			}
		})
	}
}

func TestInlineCompression_ActuallyShrinksWire(t *testing.T) {
	withCompression(t, 16, 0)
	// A highly compressible body: the compressed+base64 wire form must be well
	// under the uncompressed body. This guards against compression silently
	// degrading to a store-only frame on real corpus.
	body := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1000))
	_, wireInline, _, colEnc := roundTripBody(t, body)
	if colEnc != BodyColumnS2 {
		t.Fatalf("expected s2, got %q", colEnc)
	}
	if len(wireInline) >= len(body)/2 {
		t.Fatalf("compression ineffective: wire %d bytes vs body %d bytes", len(wireInline), len(body))
	}
}

func TestInlineCompression_BelowFloorNotCompressed(t *testing.T) {
	withCompression(t, 4096, 0) // floor above the body size
	body := []byte(`{"k":"v"}`)
	b := NewInlineBody(body, int64(len(body)), false, "application/json")
	if b.Encoding == EncodingZstd {
		t.Fatalf("body below floor must not be zstd, got %q", b.Encoding)
	}
}

func TestInlineCompression_DisabledKeepsLegacyEncoding(t *testing.T) {
	SetInlineCompression(false, 0, 0)                                 // explicit: disabled
	body := []byte(`{"content":"` + strings.Repeat("x", 2000) + `"}`) // valid JSON
	b := NewInlineBody(body, int64(len(body)), false, "application/json")
	if b.Encoding == EncodingZstd {
		t.Fatalf("compression disabled but encoding=zstd")
	}
	if b.Encoding != EncodingRaw {
		t.Fatalf("expected raw for valid JSON, got %q", b.Encoding)
	}
}

func TestDecodeBodyForColumn_ZstdMalformedReturnsNil(t *testing.T) {
	// BYTEA column form: "zstd" bytes are a RAW frame, decompressed directly.
	// Arbitrary non-frame bytes must decode to nil (treated as absent), never error.
	if got := DecodeBodyForColumn([]byte("!!!not a zstd frame!!!"), BodyColumnZstd); got != nil {
		t.Fatalf("non-frame bytes should decode to nil, got %d bytes", len(got))
	}
	if got := DecodeBodyForColumn([]byte("hello"), BodyColumnZstd); got != nil {
		t.Fatalf("short non-frame bytes should decode to nil, got %d bytes", len(got))
	}
}

func TestInlineCompression_ConfiguredLevelRoundTrips(t *testing.T) {
	// A non-default level exercises acquireEncoder's level path and must still
	// round-trip losslessly.
	for _, lvl := range []int{1, 3, 9} {
		SetInlineCompression(true, 64, lvl)
		body := []byte(`{"data":"` + strings.Repeat("payload ", 300) + `"}`)
		recovered, _, _, colEnc := roundTripBody(t, body)
		if colEnc != BodyColumnS2 {
			t.Fatalf("level %d: encoding=%q, want s2", lvl, colEnc)
		}
		if !bytes.Equal(recovered, body) {
			t.Fatalf("level %d: round-trip mismatch", lvl)
		}
	}
	SetInlineCompression(false, 0, 0)
}

func TestBody_SpillKindRoundTrips(t *testing.T) {
	// Covers the BodySpill Marshal/Unmarshal branches.
	ref := &SpillRef{Backend: "s3", Key: "k/1", Size: 4096, SHA256: "abc", ContentType: "application/json"}
	b := NewSpillBody(ref, 4096, true, "application/json")
	wire, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal spill: %v", err)
	}
	var got Body
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal spill: %v", err)
	}
	if got.Kind != BodySpill || got.SpillRef == nil || got.SpillRef.Key != "k/1" {
		t.Fatalf("spill round-trip lost the ref: %+v", got)
	}
}

func TestBody_ZstdUnmarshalRejectsNonStringInlineBytes(t *testing.T) {
	// encoding=zstd but inlineBytes is a JSON object, not a string → error branch.
	wire := []byte(`{"kind":"inline","encoding":"zstd","inlineBytes":{"not":"a string"}}`)
	var got Body
	if err := json.Unmarshal(wire, &got); err == nil {
		t.Fatal("expected error for zstd inlineBytes that is not a JSON string")
	}
}

func TestColumnPayload_NonZstdUsesEncodeBodyForColumn(t *testing.T) {
	// A plain inline body (no compression) must persist via EncodeBodyForColumn.
	SetInlineCompression(false, 0, 0)
	body := []byte("plain text body")
	b := NewInlineBody(body, int64(len(body)), false, "text/plain")
	payload, enc := b.ColumnPayload()
	if enc == BodyColumnZstd {
		t.Fatalf("non-zstd body got zstd column encoding")
	}
	if !bytes.Equal(payload, body) {
		t.Fatalf("text body should persist verbatim, got %q", payload)
	}
}
