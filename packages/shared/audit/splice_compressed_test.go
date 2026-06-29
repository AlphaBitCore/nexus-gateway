package audit

import (
	"bytes"
	"encoding/json"
	"testing"
)

// splice path (AppendInlineForSplice) must render byte-equivalent to the
// non-spliced MarshalJSON branch for compressed encodings, and round-trip
// losslessly through the column decode.
func TestSplice_Compressed_EquivAndRoundTrip(t *testing.T) {
	orig := bytes.Repeat([]byte("the quick brown fox 0123456789 "), 200) // ~6KB, >minBytes
	for _, enc := range []BodyEncoding{EncodingS2, EncodingZstd} {
		// (a) non-spliced MarshalJSON
		nonSplice, err := (Body{Kind: BodyInline, Encoding: enc, InlineBytes: orig}).MarshalJSON()
		if err != nil {
			t.Fatalf("%s MarshalJSON: %v", enc, err)
		}
		// (b) spliced render of the body value
		spliced := AppendInlineForSplice(nil, orig, enc)
		// extract the inlineBytes JSON string from the non-splice envelope
		var probe struct {
			InlineBytes json.RawMessage `json:"inlineBytes"`
		}
		if err := json.Unmarshal(nonSplice, &probe); err != nil {
			t.Fatalf("%s probe: %v", enc, err)
		}
		// Both are a quoted base64 string on the wire; the BYTEA column holds the
		// RAW frame, so base64-decode (as the Hub does at ingest) then column-decode.
		frame, ok := base64DecodeFrame(unquote(t, spliced))
		if !ok {
			t.Fatalf("%s: spliced base64 decode failed", enc)
		}
		col := DecodeBodyForColumn(frame, columnFor(enc))
		if !bytes.Equal(col, orig) {
			t.Errorf("%s: spliced body did not round-trip to original (len got=%d want=%d)", enc, len(col), len(orig))
		}
		frame2, ok := base64DecodeFrame(unquote(t, []byte(probe.InlineBytes)))
		if !ok {
			t.Fatalf("%s: non-spliced base64 decode failed", enc)
		}
		col2 := DecodeBodyForColumn(frame2, columnFor(enc))
		if !bytes.Equal(col2, orig) {
			t.Errorf("%s: non-spliced body did not round-trip", enc)
		}
	}
}

func columnFor(enc BodyEncoding) string {
	if enc == EncodingS2 {
		return BodyColumnS2
	}
	return BodyColumnZstd
}

func unquote(t *testing.T, b []byte) []byte {
	t.Helper()
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unquote: %v (%q)", err, b)
	}
	return []byte(s)
}
