package audit

import (
	"bytes"
	"strings"
	"testing"
)

func TestS2_RoundTrip_Lossless(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello world"}]}`),
		bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 2000), // ~88KB repetitive
		[]byte(""),
		[]byte("a"),
		append([]byte("binary\x00\x01\x02"), bytes.Repeat([]byte{0xff, 0x00}, 100)...),
	}
	for i, src := range cases {
		// Wire form is base64 of the S2 frame; the BYTEA column holds the RAW frame
		// (the Hub base64-decodes the wire form at ingest).
		b64 := compressInlineS2ToBase64(nil, src)
		frame, ok := base64DecodeFrame(b64)
		if !ok {
			t.Errorf("case %d: base64DecodeFrame failed", i)
			continue
		}
		got := decompressInlineS2(frame)
		if !bytes.Equal(got, src) {
			t.Errorf("case %d: round-trip mismatch (len src=%d got=%d)", i, len(src), len(got))
		}
		// also via the column path (the CP-view decode dispatch): column = raw frame
		col := DecodeBodyForColumn(frame, BodyColumnS2)
		if !bytes.Equal(col, src) {
			t.Errorf("case %d: DecodeBodyForColumn(s2) mismatch", i)
		}
	}
	// compression actually shrinks repetitive data
	big := bytes.Repeat([]byte("compress me "), 5000)
	if len(compressInlineS2ToBase64(nil, big)) >= len(big) {
		t.Errorf("S2 did not shrink repetitive data")
	}
	_ = strings.TrimSpace
}
