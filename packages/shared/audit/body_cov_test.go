package audit

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// TestColumnPayload_RawFrameStoredVerbatim covers ColumnPayload's inlineIsRawFrame
// branches for both compressed encodings: a body produced by the binary-wire
// decoder already holds the RAW frame, so ColumnPayload must store it verbatim
// (no base64-decode) and tag the matching column encoding. The stored frame must
// round-trip back to the original via DecodeBodyForColumn.
func TestColumnPayload_RawFrameStoredVerbatim(t *testing.T) {
	orig := []byte(strings.Repeat("verbatim frame payload ", 200))

	for _, tc := range []struct {
		enc     BodyEncoding
		wantCol string
		frameFn func([]byte) []byte
		decode  func([]byte) []byte
	}{
		{EncodingS2, BodyColumnS2, func(b []byte) []byte { return compressInlineS2Raw(nil, b) }, decompressInlineS2},
		{EncodingZstd, BodyColumnZstd, func(b []byte) []byte { return compressInlineZstdRaw(nil, b) }, decompressInline},
	} {
		frame := tc.frameFn(orig)
		b := Body{Kind: BodyInline, Encoding: tc.enc, InlineBytes: frame, inlineIsRawFrame: true}
		payload, col := b.ColumnPayload()
		if col != tc.wantCol {
			t.Fatalf("%s: column encoding %q want %q", tc.enc, col, tc.wantCol)
		}
		if !bytes.Equal(payload, frame) {
			t.Fatalf("%s: raw-frame body must store the frame verbatim", tc.enc)
		}
		if got := tc.decode(payload); !bytes.Equal(got, orig) {
			t.Fatalf("%s: stored frame did not decode back to original", tc.enc)
		}
	}
}

// TestColumnPayload_MalformedWireBase64FallsBackToBinary covers the fail-soft
// branch: a compressed body whose InlineBytes is NOT valid base64 (must-not-happen
// on the real wire) falls back to storing the bytes verbatim tagged "binary" so the
// audit row is never lost.
func TestColumnPayload_MalformedWireBase64FallsBackToBinary(t *testing.T) {
	bad := []byte("!!! not valid base64 !!!")
	for _, enc := range []BodyEncoding{EncodingS2, EncodingZstd} {
		b := Body{Kind: BodyInline, Encoding: enc, InlineBytes: bad} // inlineIsRawFrame=false
		payload, col := b.ColumnPayload()
		if col != BodyColumnBinary {
			t.Fatalf("%s: malformed wire base64 should fall back to %q, got %q", enc, BodyColumnBinary, col)
		}
		if !bytes.Equal(payload, bad) {
			t.Fatalf("%s: fallback must store the bytes verbatim", enc)
		}
	}
}

// TestColumnPayload_S2Zstd_DecodesWireBase64 covers the happy base64-decode branch
// of ColumnPayload for both compressed encodings (inlineIsRawFrame=false, valid
// base64-of-frame as delivered by the JSON wire path).
func TestColumnPayload_S2Zstd_DecodesWireBase64(t *testing.T) {
	orig := []byte(strings.Repeat("wire base64 frame ", 200))

	s2Wire := compressInlineS2ToBase64(nil, orig)
	bS2 := Body{Kind: BodyInline, Encoding: EncodingS2, InlineBytes: s2Wire}
	payS2, colS2 := bS2.ColumnPayload()
	if colS2 != BodyColumnS2 {
		t.Fatalf("s2 wire: column %q want %q", colS2, BodyColumnS2)
	}
	if got := DecodeBodyForColumn(payS2, colS2); !bytes.Equal(got, orig) {
		t.Fatal("s2 wire base64 path did not round-trip")
	}

	zWire := compressInlineToBase64(nil, orig)
	bZ := Body{Kind: BodyInline, Encoding: EncodingZstd, InlineBytes: zWire}
	payZ, colZ := bZ.ColumnPayload()
	if colZ != BodyColumnZstd {
		t.Fatalf("zstd wire: column %q want %q", colZ, BodyColumnZstd)
	}
	if got := DecodeBodyForColumn(payZ, colZ); !bytes.Equal(got, orig) {
		t.Fatal("zstd wire base64 path did not round-trip")
	}
}

// TestUnquoteInlineString_BackslashFallsBackToFullUnmarshal covers the fallback
// branch: when the quoted token contains a backslash escape, unquoteInlineString
// must NOT use the fast quote-strip (which would keep the literal escape bytes) and
// instead route through json.Unmarshal so the value is correctly unescaped.
func TestUnquoteInlineString_BackslashFallsBackToFullUnmarshal(t *testing.T) {
	// "a\nb" as a JSON string token: the fast path would yield the literal bytes
	// a \ n b; the correct unescape yields a, newline, b.
	got, err := unquoteInlineString([]byte(`"a\nb"`))
	if err != nil {
		t.Fatalf("unquoteInlineString: %v", err)
	}
	if string(got) != "a\nb" {
		t.Fatalf("backslash token not unescaped: got %q want %q", got, "a\nb")
	}

	// A non-string token (leading non-quote) must also route to json.Unmarshal and
	// surface its error rather than silently succeeding.
	if _, err := unquoteInlineString([]byte(`42`)); err == nil {
		t.Fatal("expected error for a non-string JSON token")
	}

	// Fast path sanity: an escape-free quoted token strips cleanly without unescape.
	fast, err := unquoteInlineString([]byte(`"aGVsbG8="`))
	if err != nil || string(fast) != "aGVsbG8=" {
		t.Fatalf("escape-free fast path: got %q err %v", fast, err)
	}
}

// TestDetachForSplice_UnknownEncodingSkipped covers the default branch of
// DetachForSplice: a large inline body whose encoding is none of the known set
// must NOT be detached (ok=false) and must NOT be armed with a marker.
func TestDetachForSplice_UnknownEncodingSkipped(t *testing.T) {
	big := bytes.Repeat([]byte("a"), SpliceMinBodyBytes+10)
	b := Body{Kind: BodyInline, Encoding: BodyEncoding("xz"), InlineBytes: big}
	real, enc, ok := b.DetachForSplice([]byte(`"M"`))
	if ok || real != nil || enc != "" {
		t.Fatalf("unknown encoding must not detach: ok=%v enc=%q", ok, enc)
	}
	if b.spliceMarker != nil {
		t.Fatal("unknown-encoding body must not be armed with a marker")
	}
}

// TestMarshalUnmarshal_S2WireRoundTrip drives the EncodingS2 branches of MarshalJSON
// (compress original → base64-of-s2 string) and UnmarshalJSON (verbatim unquote),
// then the column path back to the original — covering the S2-specific lines the
// zstd-only tests miss.
func TestMarshalUnmarshal_S2WireRoundTrip(t *testing.T) {
	orig := []byte(strings.Repeat(`{"role":"user","content":"hello s2 world"}`+"\n", 200))
	body := Body{Kind: BodyInline, Encoding: EncodingS2, InlineBytes: orig, SizeBytes: int64(len(orig)), ContentType: "application/json"}
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var got Body
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if got.Encoding != EncodingS2 {
		t.Fatalf("encoding drifted: %q", got.Encoding)
	}
	payload, col := got.ColumnPayload()
	if col != BodyColumnS2 {
		t.Fatalf("column encoding %q want %q", col, BodyColumnS2)
	}
	if recovered := DecodeBodyForColumn(payload, col); !bytes.Equal(recovered, orig) {
		t.Fatal("s2 wire round-trip did not recover original bytes")
	}
}

// TestUnmarshal_S2NonStringInlineBytesErrors covers the EncodingS2 error branch of
// UnmarshalJSON: a non-string inlineBytes value must surface an explicit error.
func TestUnmarshal_S2NonStringInlineBytesErrors(t *testing.T) {
	var b Body
	err := json.Unmarshal([]byte(`{"kind":"inline","encoding":"s2","inlineBytes":{"x":1}}`), &b)
	if err == nil {
		t.Fatal("expected error for s2 inlineBytes that is not a JSON string")
	}
	if !strings.Contains(err.Error(), "s2") {
		t.Fatalf("error %q should mention s2", err.Error())
	}
}

// TestMarshalJSON_EmptyEncodingDefaultsToBase64 covers the empty-encoding fix-up
// in MarshalJSON: a BodyInline with Encoding=="" must marshal as base64 AND stamp
// the wire encoding to "base64" so the consumer round-trips it (an empty encoding
// would otherwise be ambiguous). Verified by a full round-trip of binary bytes.
func TestMarshalJSON_EmptyEncodingDefaultsToBase64(t *testing.T) {
	raw := []byte{0x00, 0xff, 0x1b, 0x80, 0x00}
	b := Body{Kind: BodyInline, Encoding: "", InlineBytes: raw, SizeBytes: int64(len(raw))}
	wire, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	// The wire must declare encoding=base64 (the fix-up), not the empty input.
	var probe struct {
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(wire, &probe); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Encoding != string(EncodingBase64) {
		t.Fatalf("empty encoding should be stamped base64 on the wire, got %q", probe.Encoding)
	}
	var got Body
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !bytes.Equal(got.InlineBytes, raw) {
		t.Fatalf("empty-encoding body did not round-trip: got %x want %x", got.InlineBytes, raw)
	}
}

// TestUnmarshalJSON_MalformedEnvelopeErrors covers the top-level probe-unmarshal
// error branch: a body whose OUTER JSON is not an object at all must surface the
// decode error, never a zero-value Body.
func TestUnmarshalJSON_MalformedEnvelopeErrors(t *testing.T) {
	var b Body
	// A bare JSON array cannot unmarshal into the probe struct.
	if err := json.Unmarshal([]byte(`[1,2,3]`), &b); err == nil {
		t.Fatal("expected error decoding a non-object envelope into Body")
	}
}

// TestUnmarshalJSON_TextNonStringInlineBytesErrors covers the EncodingText error
// branch: encoding=text but inlineBytes is a JSON number, not a string, must error.
func TestUnmarshalJSON_TextNonStringInlineBytesErrors(t *testing.T) {
	var b Body
	err := json.Unmarshal([]byte(`{"kind":"inline","encoding":"text","inlineBytes":123}`), &b)
	if err == nil {
		t.Fatal("expected error for text inlineBytes that is not a JSON string")
	}
	if !strings.Contains(err.Error(), "text") {
		t.Fatalf("error %q should mention text payload", err.Error())
	}
}

// TestNewInlineBody_S2CodecPath covers the AI_GATEWAY_AUDIT_CODEC=s2 branch of
// NewInlineBody (Encoding=S2 chosen for a large body when compression is on). The
// codec selection is a process-global sync.Once, so this only asserts the branch
// when the env selects s2 for this process; otherwise it confirms the zstd default
// branch. Either way a real encoding decision is asserted, not a no-op.
func TestNewInlineBody_S2CodecPath(t *testing.T) {
	t.Cleanup(func() { SetInlineCompression(false, 0, 0) })
	SetInlineCompression(true, 16, 0)
	big := []byte(strings.Repeat("compress this body ", 100))
	b := NewInlineBody(big, int64(len(big)), false, "application/json")
	if inlineCodecS2() {
		if b.Encoding != EncodingS2 {
			t.Fatalf("AI_GATEWAY_AUDIT_CODEC=s2 set but encoding=%q", b.Encoding)
		}
	} else {
		if b.Encoding != EncodingZstd {
			t.Fatalf("default codec should choose zstd, got %q", b.Encoding)
		}
	}
}
