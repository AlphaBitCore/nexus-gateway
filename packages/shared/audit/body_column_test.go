package audit_test

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TestEncodeDecodeBodyForColumn_RoundTrip is the audit-fidelity golden: every
// captured body classifies correctly AND round-trips byte-for-byte through the
// BYTEA column form. The NUL-in-SSE case is load-bearing — ChatGPT emits NUL
// inside otherwise-valid UTF-8; a BYTEA column stores it raw (tagged "binary"),
// where the old TEXT column rejected a raw NUL (SQLSTATE 22021) and had to base64
// it. The "text" tag still requires NUL-free UTF-8 so renderers can trust it.
func TestEncodeDecodeBodyForColumn_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		in      []byte
		wantEnc string
	}{
		{"valid json request", []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), audit.BodyColumnText},
		{"sse response utf8 not json", []byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"), audit.BodyColumnText},
		{"plain text", []byte("upstream timeout"), audit.BodyColumnText},
		{"multibyte utf8", []byte("café ☕ 日本語"), audit.BodyColumnText},
		{"nul inside sse", []byte("data: ab\x00cd\n\n"), audit.BodyColumnBinary},
		{"invalid utf8 binary", []byte{0xff, 0xfe, 0x00, 0x01, 0x80}, audit.BodyColumnBinary},
		{"lone surrogate", []byte{0xed, 0xa0, 0x80}, audit.BodyColumnBinary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, enc := audit.EncodeBodyForColumn(tc.in)
			if enc != tc.wantEnc {
				t.Fatalf("encoding = %q, want %q", enc, tc.wantEnc)
			}
			if enc == audit.BodyColumnText && bytes.IndexByte(payload, 0) >= 0 {
				t.Fatal("text-tagged payload contains a NUL byte (renderers trust text = NUL-free UTF-8)")
			}
			got := audit.DecodeBodyForColumn(payload, enc)
			if !bytes.Equal(got, tc.in) {
				t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, tc.in)
			}
		})
	}
}

// TestDecodeBodyForColumn_MalformedBase64 confirms an unreadable base64 column
// yields nil (treated as an absent body), never a panic or a surfaced error.
func TestDecodeBodyForColumn_MalformedBase64(t *testing.T) {
	if got := audit.DecodeBodyForColumn([]byte("not%%base64"), audit.BodyColumnBase64); got != nil {
		t.Fatalf("malformed base64 decoded to %q, want nil", got)
	}
}

// TestDecodeBodyForColumn_EmptyEncodingIsVerbatim confirms a body with an empty
// or unknown encoding is returned verbatim (defensive default = text).
func TestDecodeBodyForColumn_EmptyEncodingIsVerbatim(t *testing.T) {
	body := []byte(`{"a":1}`)
	if got := audit.DecodeBodyForColumn(body, ""); !bytes.Equal(got, body) {
		t.Fatalf("empty encoding: got %q, want verbatim %q", got, body)
	}
	// Sanity: a real base64 string under base64 encoding decodes.
	enc := base64.StdEncoding.EncodeToString(body)
	if got := audit.DecodeBodyForColumn([]byte(enc), audit.BodyColumnBase64); !bytes.Equal(got, body) {
		t.Fatalf("base64 decode: got %q, want %q", got, body)
	}
}
