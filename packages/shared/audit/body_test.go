package audit_test

import (
	"bytes"
	"encoding/json"
	"testing"

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

func TestBody_RawEncodingRejectsInvalidJSON(t *testing.T) {
	// Caller explicitly chose raw but bytes aren't valid JSON.
	bad := audit.Body{Kind: audit.BodyInline, Encoding: audit.EncodingRaw, InlineBytes: []byte("not json")}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("expected error when raw encoding has invalid JSON bytes")
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
	// Auto-detected encoding: valid JSON ⇒ raw, anything else ⇒ base64.
	cases := []struct {
		in   []byte
		want audit.BodyEncoding
	}{
		{[]byte(`"plain string"`), audit.EncodingRaw},
		{[]byte(`{"a":1}`), audit.EncodingRaw},
		{[]byte(`[1,2,3]`), audit.EncodingRaw},
		{[]byte(`true`), audit.EncodingRaw},
		{[]byte(`null`), audit.EncodingRaw},
		{[]byte("event: delta\n"), audit.EncodingBase64},
		{[]byte{0x00, 0x01, 0x02}, audit.EncodingBase64},
	}
	for _, c := range cases {
		body := audit.NewInlineBody(c.in, int64(len(c.in)), false, "")
		if body.Encoding != c.want {
			t.Errorf("encoding for %q: got %q want %q", string(c.in), body.Encoding, c.want)
		}
	}
}
