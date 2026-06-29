package audit

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// TestAppendJSONString_MatchesGoccy pins the splice text escaper to goccy's string
// encoding byte-for-byte — goccy, NOT stdlib, is the encoder the audit path uses,
// and goccy diverges from stdlib on backspace/form-feed (u-escapes, not \b/\f).
// Covered: quotes, backslash, the short escapes, C0 controls (incl. backspace 0x08
// and form-feed 0x0c), the HTML trio < > &, the JS line separators U+2028 / U+2029,
// multibyte UTF-8, and an invalid UTF-8 byte (replacement char). Byte-identity
// matters because a record may be marshaled via the spliced path or the plain
// path and both must yield the same wire string.
func TestAppendJSONString_MatchesGoccy(t *testing.T) {
	cases := map[string]string{
		"plain":         "hello world",
		"quote":         `she said "hi"`,
		"backslash":     `a\b\c`,
		"newline":       "line1\nline2\r\n\ttab",
		"backspace_ff":  "a\bb\fc", // 0x08, 0x0c
		"controls":      "\x00\x01\x1f end",
		"html":          "<script>x && y > z</script>",
		"unicode":       "héllo 世界 \U0001F680",
		"js_line_seps":  "a b c",
		"sse_realistic": "data: {\"delta\":{\"content\":\"<b>hi & bye</b>\"}}\n\n",
		"empty":         "",
		"only_specials": "\"\\\n\r\t<>&",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			want, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			got := appendJSONString(nil, []byte(s))
			if !bytes.Equal(got, want) {
				t.Fatalf("escape mismatch\n got=%s\nwant=%s", got, want)
			}
		})
	}
	// Invalid UTF-8 byte -> goccy emits the U+FFFD replacement escape.
	got := appendJSONString(nil, []byte{0xff, 'a'})
	want, _ := json.Marshal(string([]byte{0xff, 'a'}))
	if !bytes.Equal(got, want) {
		t.Fatalf("invalid-utf8 escape mismatch\n got=%s\nwant=%s", got, want)
	}
}

// TestAppendJSONString_AppendsToExisting verifies the escaper appends to a
// non-empty destination rather than overwriting it (the splice buffer already
// holds the envelope prefix when this is called).
func TestAppendJSONString_AppendsToExisting(t *testing.T) {
	dst := []byte("PREFIX")
	dst = appendJSONString(dst, []byte("x\ny"))
	if got, want := string(dst), `PREFIX"x\ny"`; got != want {
		t.Fatalf("append mismatch: got=%q want=%q", got, want)
	}
}

// TestDetachForSplice_GatesByKindSizeEncoding asserts only large inline bodies of
// a known encoding are detached; everything else is left to encode inline.
func TestDetachForSplice_GatesByKindSizeEncoding(t *testing.T) {
	big := bytes.Repeat([]byte("a"), SpliceMinBodyBytes)
	small := bytes.Repeat([]byte("a"), SpliceMinBodyBytes-1)

	t.Run("large_text_detaches", func(t *testing.T) {
		b := Body{Kind: BodyInline, Encoding: EncodingText, InlineBytes: big}
		real, enc, ok := b.DetachForSplice([]byte(`"M"`))
		if !ok || enc != EncodingText || !bytes.Equal(real, big) {
			t.Fatalf("expected detach text: ok=%v enc=%q", ok, enc)
		}
		if b.spliceMarker == nil {
			t.Fatal("spliceMarker not armed")
		}
	})
	t.Run("small_body_skipped", func(t *testing.T) {
		b := Body{Kind: BodyInline, Encoding: EncodingRaw, InlineBytes: small}
		if _, _, ok := b.DetachForSplice([]byte(`"M"`)); ok {
			t.Fatal("small body should not detach")
		}
		if b.spliceMarker != nil {
			t.Fatal("small body must not arm marker")
		}
	})
	t.Run("spill_skipped", func(t *testing.T) {
		b := Body{Kind: BodySpill, InlineBytes: big}
		if _, _, ok := b.DetachForSplice([]byte(`"M"`)); ok {
			t.Fatal("spill body should not detach")
		}
	})
}

// TestSpliceRoundTrip_AllEncodings is the end-to-end regression: for every
// inline encoding, a detached body marshaled-with-marker and then spliced back via
// AppendInlineForSplice must decode (Body.UnmarshalJSON) to the EXACT original
// bytes — proving the splice is a pure allocation optimization, never a data change.
func TestSpliceRoundTrip_AllEncodings(t *testing.T) {
	marker := []byte(`"__splice_marker_test__"`)
	cases := []struct {
		name string
		body []byte
	}{
		{"raw_json", []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 512) + `"}]}`)},
		{"text_sse", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"<x> & </x>\"}}]}\n\n" + strings.Repeat("event: ping\n", 64))},
		{"text_with_specials", []byte(strings.Repeat("a", 200) + "\n\t\"\\  done")},
		{"base64_binary", append([]byte{0x00, 0x01, 0xff, 0xfe}, bytes.Repeat([]byte{0x00, 0x80}, 256)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := NewInlineBody(tc.body, int64(len(tc.body)), false, "application/octet-stream")
			b := orig // copy; DetachForSplice mutates the copy
			real, enc, ok := b.DetachForSplice(marker)
			if !ok {
				t.Fatalf("DetachForSplice !ok (encoding=%q len=%d)", orig.Encoding, len(tc.body))
			}
			// Marshal the marker-armed body (tiny: emits the marker, not the body).
			wire, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("marshal armed body: %v", err)
			}
			if !bytes.Contains(wire, marker) {
				t.Fatalf("armed wire missing marker: %s", wire)
			}
			// Splice the real bytes in place of the marker.
			idx := bytes.Index(wire, marker)
			out := append([]byte{}, wire[:idx]...)
			out = AppendInlineForSplice(out, real, enc)
			out = append(out, wire[idx+len(marker):]...)

			// Decode and compare to the original captured bytes.
			var got Body
			if err := got.UnmarshalJSON(out); err != nil {
				t.Fatalf("decode spliced wire: %v\nwire=%s", err, out)
			}
			if !bytes.Equal(got.InlineBytes, tc.body) {
				t.Fatalf("splice corrupted body\n got=%x\nwant=%x", got.InlineBytes, tc.body)
			}
			if got.Encoding != orig.Encoding {
				t.Fatalf("encoding drifted: got=%q want=%q", got.Encoding, orig.Encoding)
			}
		})
	}
}

// TestSpliceMatchesPlainWire: for a text/base64 body, the spliced wire must be
// byte-identical to the plain (un-spliced) MarshalJSON wire — the splice changes
// allocation behavior, not output.
func TestSpliceMatchesPlainWire(t *testing.T) {
	marker := []byte(`"__splice_marker_test__"`)
	bodies := [][]byte{
		[]byte("data: hello <world> & \"friends\"\n\n" + strings.Repeat("x", 300)),
		// Text body (UTF-8, no NUL) exercising every escape class — backspace,
		// form-feed, other C0 controls, HTML chars, and the JS line separators — so
		// goccy's plain output is the byte-identity oracle for the splice escaper.
		[]byte("ctl\b\f\x01\x1f html<>& seps   end " + strings.Repeat("y", 200)),
		append([]byte{0x00, 0xff}, bytes.Repeat([]byte{0x10, 0x80}, 200)...),
	}
	for _, body := range bodies {
		plainBody := NewInlineBody(body, int64(len(body)), false, "")
		plainWire, err := json.Marshal(plainBody)
		if err != nil {
			t.Fatalf("plain marshal: %v", err)
		}
		armed := plainBody
		real, enc, ok := armed.DetachForSplice(marker)
		if !ok {
			t.Fatal("detach !ok")
		}
		armedWire, _ := json.Marshal(armed)
		idx := bytes.Index(armedWire, marker)
		spliced := append([]byte{}, armedWire[:idx]...)
		spliced = AppendInlineForSplice(spliced, real, enc)
		spliced = append(spliced, armedWire[idx+len(marker):]...)
		if !bytes.Equal(spliced, plainWire) {
			t.Fatalf("spliced wire != plain wire (encoding=%q)\n spliced=%s\n   plain=%s", enc, spliced, plainWire)
		}
	}
}
