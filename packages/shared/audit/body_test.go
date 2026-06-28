package audit_test

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// Each row exercises a distinct kind/encoding combination plus the
// non-JSON-bytes round-trip that motivated this redesign.
func TestBody_RoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		input  audit.Body
		expect audit.BodyKind
	}{
		{"absent", audit.EmptyBody(), audit.BodyAbsent},
		{"inline_json", audit.NewInlineBody([]byte(`{"hello":"world"}`), 17, false, "application/json"), audit.BodyInline},
		{"inline_sse_with_esc", audit.NewInlineBody([]byte("event: delta\ndata: \x1b[36m\"hi\"\n\n"), 30, false, "text/event-stream"), audit.BodyInline},
		{"inline_binary", audit.NewInlineBody([]byte{0xff, 0x00, 0x1b, 0x7f, 0x80}, 5, false, "application/octet-stream"), audit.BodyInline},
		{"inline_empty_collapses_to_absent", audit.NewInlineBody(nil, 0, false, ""), audit.BodyAbsent},
		{"spill", audit.NewSpillBody(&audit.SpillRef{Backend: "localfs", Key: "2026-04-28/abc-req.bin", Size: 1234, SHA256: audit.SHA256Hex([]byte("payload")), ContentType: "application/json"}, 1234, false, "application/json"), audit.BodySpill},
		{"spill_truncated", audit.NewSpillBody(&audit.SpillRef{Backend: "localfs", Key: "k", Size: 256 << 20}, 300<<20, true, "application/octet-stream"), audit.BodySpill},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got audit.Body
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Kind != tc.expect {
				t.Fatalf("kind: got %q want %q", got.Kind, tc.expect)
			}
			if !bytes.Equal(got.InlineBytes, tc.input.InlineBytes) {
				t.Fatalf("inline bytes mismatch: got %v want %v", got.InlineBytes, tc.input.InlineBytes)
			}
			if (got.SpillRef == nil) != (tc.input.SpillRef == nil) {
				t.Fatalf("spill ref presence mismatch: got %v want %v", got.SpillRef, tc.input.SpillRef)
			}
			if got.SpillRef != nil && tc.input.SpillRef != nil {
				if *got.SpillRef != *tc.input.SpillRef {
					t.Fatalf("spill ref mismatch: got %+v want %+v", *got.SpillRef, *tc.input.SpillRef)
				}
			}
			if got.SizeBytes != tc.input.SizeBytes {
				t.Fatalf("size: got %d want %d", got.SizeBytes, tc.input.SizeBytes)
			}
			if got.Truncated != tc.input.Truncated {
				t.Fatalf("truncated: got %v want %v", got.Truncated, tc.input.Truncated)
			}
		})
	}
}

// TestBody_CompressedRoundTripUnquoteFastPath locks the Hub-ingest decode
// optimization: a large body marked for s2/zstd compression marshals to a
// base64-of-frame JSON string, and UnmarshalJSON must recover the ORIGINAL bytes
// exactly via the quote-stripping fast path (which skips json.Unmarshal's per-byte
// unescape because base64 can never contain a JSON escape). Both codecs are
// exercised; the assertion is byte-exact recovery, not merely "no error".
func TestBody_CompressedRoundTripUnquoteFastPath(t *testing.T) {
	// A body comfortably above the compression floor with high-entropy + repeating
	// regions so the frame is non-trivial yet the wire string stays pure base64.
	orig := make([]byte, 64*1024)
	for i := range orig {
		orig[i] = byte((i*31 + 7) % 251)
	}
	// Drive both codecs deterministically by constructing the Body with the target
	// encoding directly — MarshalJSON compresses InlineBytes to a base64-of-frame
	// string for s2/zstd, which is exactly the wire form the fast path must invert.
	// (NewInlineBody's codec pick is process-global via sync.Once, so direct
	// construction is the only way to exercise both codecs in one test run.)
	for _, enc := range []audit.BodyEncoding{audit.EncodingZstd, audit.EncodingS2} {
		t.Run(string(enc), func(t *testing.T) {
			audit.SetInlineCompression(true, 1024, 3)
			defer audit.SetInlineCompression(false, 0, 0)

			body := audit.Body{Kind: audit.BodyInline, Encoding: enc, InlineBytes: orig, SizeBytes: int64(len(orig)), ContentType: "application/json"}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got audit.Body
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// InlineBytes after UnmarshalJSON is the base64-of-frame string bytes
			// (the Hub persists it verbatim into the BYTEA column). The column path
			// (base64-decode to the raw frame, then decompress) must reproduce the
			// original body byte-for-byte.
			payload, enc := got.ColumnPayload()
			decoded := audit.DecodeBodyForColumn(payload, enc)
			if !bytes.Equal(decoded, orig) {
				t.Fatalf("decoded body mismatch: got %d bytes want %d", len(decoded), len(orig))
			}
		})
	}
}

func TestBody_RawEncodingRejectsInvalidJSON(t *testing.T) {
	// Caller explicitly chose raw but bytes aren't valid JSON. This body is
	// built by direct struct literal (no constructor), so rawValidated is false
	// and MarshalJSON MUST still run the defensive json.Valid re-scan and error.
	bad := audit.Body{Kind: audit.BodyInline, Encoding: audit.EncodingRaw, InlineBytes: []byte("not json")}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("expected error when raw encoding has invalid JSON bytes")
	}
}

// BenchmarkBody_MarshalJSON_RawLarge documents the constructor-trust win: a
// large raw body built via NewInlineBody must marshal WITHOUT paying a second
// O(len) json.Valid scan (the re-scan measured ~20% of gateway CPU under load).
// Run with -benchmem; the win shows as flat ns/op as the body grows because the
// marshal no longer walks every byte to re-validate.
func BenchmarkBody_MarshalJSON_RawLarge(b *testing.B) {
	// ~50 KB of valid JSON, the size of a typical captured chat body.
	buf := make([]byte, 0, 50*1024)
	buf = append(buf, '{')
	for i := range 1500 {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, []byte(`"k0000000000":"vvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"`)...)
	}
	buf = append(buf, '}')
	if !json.Valid(buf) {
		b.Fatalf("benchmark fixture is not valid JSON")
	}
	body := audit.NewInlineBody(buf, int64(len(buf)), false, "application/json")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := json.Marshal(body); err != nil {
			b.Fatalf("marshal: %v", err)
		}
	}
}

func TestBody_NewSpillBodyNilRefReturnsEmpty(t *testing.T) {
	// Constructor guard: nil SpillRef MUST collapse to an empty body,
	// otherwise downstream MarshalJSON would later panic / error out.
	b := audit.NewSpillBody(nil, 1000, false, "application/json")
	if b.Kind != audit.BodyAbsent {
		t.Errorf("nil SpillRef should produce BodyAbsent, got %q", b.Kind)
	}
	if b.SpillRef != nil {
		t.Errorf("nil ref must remain nil in result: %+v", b.SpillRef)
	}
}

func TestBody_MarshalSpillNilRefErrors(t *testing.T) {
	// Direct construction with kind=spill + nil ref bypasses NewSpillBody.
	// MarshalJSON must surface this as an explicit error, not silently
	// emit a spill envelope with a null ref.
	bad := audit.Body{Kind: audit.BodySpill, SpillRef: nil}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("expected error for spill kind with nil ref")
	}
}

func TestBody_MarshalUnknownKindErrors(t *testing.T) {
	bad := audit.Body{Kind: audit.BodyKind("not-a-kind")}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestBody_MarshalUnknownEncodingErrors(t *testing.T) {
	bad := audit.Body{Kind: audit.BodyInline, Encoding: audit.BodyEncoding("xz"), InlineBytes: []byte("x")}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("expected error for unknown encoding")
	}
}

func TestBody_UnmarshalMalformedJSONErrors(t *testing.T) {
	var b audit.Body
	if err := json.Unmarshal([]byte(`{not json`), &b); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestBody_UnmarshalSpillMissingRefErrors(t *testing.T) {
	// kind=spill but no spillRef key — must surface as explicit error.
	// Otherwise downstream readers would receive a zero-value SpillRef.
	var b audit.Body
	err := json.Unmarshal([]byte(`{"kind":"spill","sizeBytes":100}`), &b)
	if err == nil {
		t.Fatal("expected error for spill without spillRef")
	}
}

func TestBody_UnmarshalUnknownKindErrors(t *testing.T) {
	var b audit.Body
	err := json.Unmarshal([]byte(`{"kind":"frobnicate"}`), &b)
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestBody_UnmarshalUnknownEncodingErrors(t *testing.T) {
	var b audit.Body
	err := json.Unmarshal([]byte(`{"kind":"inline","encoding":"xz","inlineBytes":"x"}`), &b)
	if err == nil {
		t.Fatal("expected error for unknown encoding")
	}
}

func TestBody_UnmarshalBase64MalformedErrors(t *testing.T) {
	// Encoding=base64 but the inlineBytes value isn't a valid JSON
	// string (it's a number).
	var b audit.Body
	err := json.Unmarshal([]byte(`{"kind":"inline","encoding":"base64","inlineBytes":42}`), &b)
	if err == nil {
		t.Fatal("expected error when base64 inlineBytes is not a JSON string")
	}
}

func TestBody_UnmarshalBase64GarbledStringErrors(t *testing.T) {
	// Encoding=base64 but the string isn't valid base64.
	var b audit.Body
	err := json.Unmarshal([]byte(`{"kind":"inline","encoding":"base64","inlineBytes":"!!!not-base64!!!"}`), &b)
	if err == nil {
		t.Fatal("expected error when base64 string fails to decode")
	}
}

func TestBody_AutoDetectEncoding(t *testing.T) {
	// Auto-detected encoding (three-way): valid JSON ⇒ raw, valid UTF-8 non-JSON
	// ⇒ text, binary / NUL-bearing ⇒ base64.
	cases := []struct {
		in   []byte
		want audit.BodyEncoding
	}{
		{[]byte(`"plain string"`), audit.EncodingRaw},
		{[]byte(`{"a":1}`), audit.EncodingRaw},
		{[]byte(`[1,2,3]`), audit.EncodingRaw},
		{[]byte(`true`), audit.EncodingRaw},
		{[]byte(`null`), audit.EncodingRaw},
		{[]byte("event: delta\n"), audit.EncodingText},      // SSE line: UTF-8, not JSON
		{[]byte("data: {\"x\":1}\n\n"), audit.EncodingText}, // SSE frame
		{[]byte("café ☕"), audit.EncodingText},              // multibyte UTF-8
		{[]byte("data: ab\x00cd"), audit.EncodingBase64},    // embedded NUL → base64
		{[]byte{0xff, 0xfe, 0x01}, audit.EncodingBase64},    // invalid UTF-8 → base64
		{[]byte{0x00, 0x01, 0x02}, audit.EncodingBase64},    // NUL bytes → base64
	}
	for _, c := range cases {
		body := audit.NewInlineBody(c.in, int64(len(c.in)), false, "")
		if body.Encoding != c.want {
			t.Errorf("encoding for %q: got %q want %q", string(c.in), body.Encoding, c.want)
		}
	}
}

// TestNewInlineBody_ThreeWayClassification locks the json.Valid-gated hybrid:
// valid JSON ⇒ raw (verbatim splice), valid UTF-8 non-JSON ⇒ text (escaped
// string), binary / NUL ⇒ base64. The raw boundary is decided by stdlib
// encoding/json.Valid (zero-alloc); a body mis-tagged raw would later fail to
// round-trip, and a NUL mis-tagged text would be rejected by a PG text column.
func TestNewInlineBody_ThreeWayClassification(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"a":1}`), []byte(`[1,2,3]`), []byte(`"str"`), []byte(`123`),
		[]byte(`true`), []byte(`null`), []byte(`{"nested":{"x":[1,2,{"y":null}]}}`),
		[]byte(`{"unicode":"é😀"}`), []byte(`1.5e10`),
		[]byte(`data: {"x":1}`),              // SSE line — UTF-8, not JSON → text
		[]byte("\x1b[31mred\x1b[0m"),         // ANSI escape — UTF-8 control chars → text
		[]byte("partial{"), []byte(`{"a":1`), // truncated → text
		[]byte(`{"a":1}trailing`),              // trailing garbage → text
		[]byte("\x00\x01\x02"),                 // NUL bytes → base64
		[]byte{0xff, 0xfe},                     // invalid UTF-8 → base64
		[]byte("ok\x00mid"),                    // UTF-8 with embedded NUL → base64
		{}, []byte(` `), []byte(`  {"a":1}  `), // whitespace edges
	}
	for _, b := range cases {
		if len(b) == 0 {
			continue
		}
		wantEnc := audit.EncodingBase64
		switch {
		case json.Valid(b):
			wantEnc = audit.EncodingRaw
		case utf8.Valid(b) && bytes.IndexByte(b, 0) < 0:
			wantEnc = audit.EncodingText
		}
		body := audit.NewInlineBody(b, int64(len(b)), false, "")
		if body.Encoding != wantEnc {
			t.Errorf("NewInlineBody(%q).Encoding=%v want %v", b, body.Encoding, wantEnc)
		}
		// Round-trip through Marshal→Unmarshal. text/base64 carry non-JSON bytes
		// where every byte matters → byte-exact. raw is valid JSON embedded as a
		// nested value; the envelope marshal compacts insignificant whitespace
		// (same as the old JSONB storage), so raw is checked for JSON-semantic
		// equality, not byte-identity.
		raw, err := body.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%q): %v", b, err)
		}
		var rt audit.Body
		if err := rt.UnmarshalJSON(raw); err != nil {
			t.Fatalf("UnmarshalJSON(%q): %v", b, err)
		}
		if wantEnc == audit.EncodingRaw {
			if !json.Valid(rt.InlineBytes) {
				t.Errorf("raw round-trip produced invalid JSON for %q: %q", b, rt.InlineBytes)
			}
		} else if !bytes.Equal(rt.InlineBytes, b) {
			t.Errorf("round-trip mismatch for %q: got %q", b, rt.InlineBytes)
		}
	}
}

// BenchmarkNewInlineBody_ValidLarge proves the validity check is now zero-alloc
// (goccy json.Valid decoded the whole body into interface{}, ~4x body alloc).
func BenchmarkNewInlineBody_ValidLarge(b *testing.B) {
	body := []byte(`{"model":"m","messages":[`)
	for i := range 200 {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, []byte(`{"role":"user","content":"the quick brown fox jumps over the lazy dog"}`)...)
	}
	body = append(body, []byte(`]}`)...)
	b.ReportAllocs()
	for range b.N {
		_ = audit.NewInlineBody(body, int64(len(body)), false, "")
	}
}
