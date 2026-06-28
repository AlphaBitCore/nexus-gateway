package audit_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TestAppendReadBodyBinary_RoundTrip is the binary-wire fidelity golden: for every
// Body kind/encoding combination AppendBodyBinary must produce a frame that
// ReadBodyBinary parses back to an equal Body, and the reported consumed-byte
// count must equal the appended frame length (no over- or under-read). The S2 and
// Zstd cases carry the RAW compressed frame in InlineBytes after read (the
// inlineIsRawFrame contract), so they are validated by re-running the body through
// ColumnPayload + DecodeBodyForColumn back to the ORIGINAL captured bytes rather
// than by comparing the compressed frame to the original. Every primitive
// (appendLenStr / appendBool / uvarint / varint / str / byte / bool / lenBytes /
// appendLenPrefixed) and both encoding<->byte maps are exercised here.
func TestAppendReadBodyBinary_RoundTrip(t *testing.T) {
	highEntropy := make([]byte, 4096)
	for i := range highEntropy {
		highEntropy[i] = byte((i*131 + 17) % 251)
	}
	compressible := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 200))

	cases := []struct {
		name string
		// orig is the ORIGINAL captured bytes the Body was built from. For
		// non-compressed encodings it equals the Body.InlineBytes; for s2/zstd the
		// Body holds the original bytes pre-Append (Append compresses them), and the
		// recovered body's column decode must reproduce orig.
		orig     []byte
		build    func(orig []byte) audit.Body
		wantEnc  audit.BodyEncoding
		wantKind audit.BodyKind
		// compressed marks s2/zstd cases that decode via the column path.
		compressed bool
	}{
		{
			name:     "absent",
			build:    func([]byte) audit.Body { return audit.EmptyBody() },
			wantKind: audit.BodyAbsent,
		},
		{
			name:     "inline_raw_json",
			orig:     []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
			build:    func(o []byte) audit.Body { return mkInline(o, audit.EncodingRaw) },
			wantEnc:  audit.EncodingRaw,
			wantKind: audit.BodyInline,
		},
		{
			name:     "inline_text_sse",
			orig:     []byte("event: delta\ndata: \"hi\"\n\n"),
			build:    func(o []byte) audit.Body { return mkInline(o, audit.EncodingText) },
			wantEnc:  audit.EncodingText,
			wantKind: audit.BodyInline,
		},
		{
			name:     "inline_text_with_newline_0x0A_and_unicode",
			orig:     []byte("café ☕\nline2\nline3 日本語"),
			build:    func(o []byte) audit.Body { return mkInline(o, audit.EncodingText) },
			wantEnc:  audit.EncodingText,
			wantKind: audit.BodyInline,
		},
		{
			name:     "inline_base64_binary_with_0xFF_and_NUL",
			orig:     []byte{0xff, 0x00, 0x0a, 0x1b, 0x7f, 0x80, 0x00, 0xfe},
			build:    func(o []byte) audit.Body { return mkInline(o, audit.EncodingBase64) },
			wantEnc:  audit.EncodingBase64,
			wantKind: audit.BodyInline,
		},
		{
			name:     "inline_raw_empty_inlinebytes",
			orig:     nil,
			build:    func(o []byte) audit.Body { return mkInline(o, audit.EncodingRaw) },
			wantEnc:  audit.EncodingRaw,
			wantKind: audit.BodyInline,
		},
		{
			name:       "inline_s2_compressible",
			orig:       compressible,
			build:      func(o []byte) audit.Body { return mkInline(o, audit.EncodingS2) },
			wantEnc:    audit.EncodingS2,
			wantKind:   audit.BodyInline,
			compressed: true,
		},
		{
			name:       "inline_s2_high_entropy",
			orig:       highEntropy,
			build:      func(o []byte) audit.Body { return mkInline(o, audit.EncodingS2) },
			wantEnc:    audit.EncodingS2,
			wantKind:   audit.BodyInline,
			compressed: true,
		},
		{
			name:       "inline_zstd_compressible",
			orig:       compressible,
			build:      func(o []byte) audit.Body { return mkInline(o, audit.EncodingZstd) },
			wantEnc:    audit.EncodingZstd,
			wantKind:   audit.BodyInline,
			compressed: true,
		},
		{
			name:       "inline_zstd_high_entropy",
			orig:       highEntropy,
			build:      func(o []byte) audit.Body { return mkInline(o, audit.EncodingZstd) },
			wantEnc:    audit.EncodingZstd,
			wantKind:   audit.BodyInline,
			compressed: true,
		},
		{
			name: "spill_full",
			build: func([]byte) audit.Body {
				return audit.NewSpillBody(&audit.SpillRef{
					Backend:     "s3",
					Key:         "2026/abc-req.bin",
					Size:        4096,
					SHA256:      audit.SHA256Hex([]byte("payload")),
					ContentType: "application/json",
					Truncated:   true,
				}, 4096, true, "application/json")
			},
			wantKind: audit.BodySpill,
		},
		{
			name: "spill_minimal_empty_strings",
			build: func([]byte) audit.Body {
				return audit.NewSpillBody(&audit.SpillRef{Backend: "localfs", Key: "k"}, 0, false, "")
			},
			wantKind: audit.BodySpill,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.build(tc.orig)
			// Prefix dst with sentinel bytes to prove Append grows (not overwrites)
			// the destination and Read consumes only the frame portion.
			const sentinel = "SENT"
			frame := audit.AppendBodyBinary([]byte(sentinel), in)
			if !bytes.HasPrefix(frame, []byte(sentinel)) {
				t.Fatalf("AppendBodyBinary overwrote dst prefix: %q", frame[:4])
			}
			wire := frame[len(sentinel):]

			got, n, err := audit.ReadBodyBinary(wire)
			if err != nil {
				t.Fatalf("ReadBodyBinary: %v", err)
			}
			if n != len(wire) {
				t.Fatalf("consumed %d bytes, frame is %d bytes", n, len(wire))
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("kind: got %q want %q", got.Kind, tc.wantKind)
			}

			switch tc.wantKind {
			case audit.BodyAbsent:
				// nothing else to assert; absent carries no payload.
			case audit.BodySpill:
				if got.SpillRef == nil || in.SpillRef == nil {
					t.Fatalf("spill ref nil: got=%v in=%v", got.SpillRef, in.SpillRef)
				}
				if *got.SpillRef != *in.SpillRef {
					t.Fatalf("spill ref mismatch:\n got %+v\nwant %+v", *got.SpillRef, *in.SpillRef)
				}
				if got.SizeBytes != in.SizeBytes || got.Truncated != in.Truncated || got.ContentType != in.ContentType {
					t.Fatalf("spill outer fields mismatch: got{%d,%v,%q} want{%d,%v,%q}",
						got.SizeBytes, got.Truncated, got.ContentType,
						in.SizeBytes, in.Truncated, in.ContentType)
				}
			case audit.BodyInline:
				if got.Encoding != tc.wantEnc {
					t.Fatalf("encoding: got %q want %q", got.Encoding, tc.wantEnc)
				}
				if got.SizeBytes != in.SizeBytes || got.Truncated != in.Truncated || got.ContentType != in.ContentType {
					t.Fatalf("inline outer fields mismatch: got{%d,%v,%q} want{%d,%v,%q}",
						got.SizeBytes, got.Truncated, got.ContentType,
						in.SizeBytes, in.Truncated, in.ContentType)
				}
				if tc.compressed {
					// InlineBytes now holds the RAW frame; the column path stores it
					// verbatim and the view decode must reproduce the ORIGINAL bytes.
					payload, colEnc := got.ColumnPayload()
					if !bytes.Equal(payload, got.InlineBytes) {
						t.Fatalf("compressed body: ColumnPayload must store the raw frame verbatim (inlineIsRawFrame), got %d want %d", len(payload), len(got.InlineBytes))
					}
					recovered := audit.DecodeBodyForColumn(payload, colEnc)
					if !bytes.Equal(recovered, tc.orig) {
						t.Fatalf("compressed body did not recover original: got %d bytes want %d", len(recovered), len(tc.orig))
					}
				} else if !bytes.Equal(got.InlineBytes, in.InlineBytes) {
					t.Fatalf("inline bytes mismatch:\n got %x\nwant %x", got.InlineBytes, in.InlineBytes)
				}
			}
		})
	}
}

// mkInline builds a Body with a caller-chosen encoding without going through
// NewInlineBody's auto-classification, so a single test can drive each encoding
// branch of AppendBodyBinary deterministically.
// TestReadBodyBinaryMeta verifies the meta-only decode: for every body shape it
// must consume EXACTLY the same byte count as ReadBodyBinary (proving it skips
// the inline content via the length prefix, not by mis-parsing), reproduce the
// same metadata (Kind/Encoding/SizeBytes/Truncated/ContentType + spill ref), and
// leave InlineBytes nil (content not materialized).
func TestReadBodyBinaryMeta(t *testing.T) {
	bodies := []audit.Body{
		audit.EmptyBody(),
		mkInline([]byte(`{"model":"gpt-4o"}`), audit.EncodingRaw),
		mkInline([]byte("data: hi\n\nline2"), audit.EncodingText), // content has 0x0A
		mkInline([]byte{0xff, 0x00, 0x1b}, audit.EncodingBase64),
		mkInline([]byte(strings.Repeat("ab cd ", 500)), audit.EncodingS2),
		mkInline([]byte(strings.Repeat("xy ", 500)), audit.EncodingZstd),
		mkInline(nil, audit.EncodingRaw),
		audit.NewSpillBody(&audit.SpillRef{Backend: "s3", Key: "k/1", Size: 9, ContentType: "application/json", Truncated: true}, 9, true, "application/json"),
	}
	for i, in := range bodies {
		wire := audit.AppendBodyBinary(nil, in)
		full, nFull, err := audit.ReadBodyBinary(wire)
		if err != nil {
			t.Fatalf("case %d: ReadBodyBinary: %v", i, err)
		}
		meta, nMeta, err := audit.ReadBodyBinaryMeta(wire)
		if err != nil {
			t.Fatalf("case %d: ReadBodyBinaryMeta: %v", i, err)
		}
		if nMeta != nFull {
			t.Errorf("case %d (%s): meta consumed %d bytes, full consumed %d — content not skipped cleanly", i, in.Kind, nMeta, nFull)
		}
		if meta.Kind != full.Kind || meta.Encoding != full.Encoding || meta.SizeBytes != full.SizeBytes ||
			meta.Truncated != full.Truncated || meta.ContentType != full.ContentType {
			t.Errorf("case %d (%s): metadata mismatch\n meta=%+v\n full=%+v", i, in.Kind, meta, full)
		}
		if in.Kind == audit.BodyInline && meta.InlineBytes != nil {
			t.Errorf("case %d (%s): meta decode must skip content, got %d inline bytes", i, in.Kind, len(meta.InlineBytes))
		}
		if in.Kind == audit.BodySpill {
			if meta.SpillRef == nil || full.SpillRef == nil || *meta.SpillRef != *full.SpillRef {
				t.Errorf("case %d: spill ref mismatch meta=%v full=%v", i, meta.SpillRef, full.SpillRef)
			}
		}
	}
}

func mkInline(b []byte, enc audit.BodyEncoding) audit.Body {
	return audit.Body{
		Kind:        audit.BodyInline,
		Encoding:    enc,
		InlineBytes: b,
		SizeBytes:   int64(len(b)),
		Truncated:   false,
		ContentType: "application/json",
	}
}

// TestReadBodyBinary_TruncatedInput asserts each short-read failure mode surfaces
// the specific byteReader error, not a panic or a silently-truncated Body.
func TestReadBodyBinary_TruncatedInput(t *testing.T) {
	// A complete inline-raw frame to truncate at each boundary.
	full := audit.AppendBodyBinary(nil, mkInline([]byte(`{"a":1}`), audit.EncodingRaw))

	cases := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"empty_no_kind_byte", []byte{}, "short read (byte)"},
		{"kind_inline_but_no_encoding_byte", []byte{0x01}, "short read (byte)"},
		{"unknown_kind_byte", []byte{0x09}, "unknown body kind byte"},
		{"inline_unknown_encoding_byte", []byte{0x01, 0x09}, "unknown inline encoding byte"},
		// kind=inline, enc=raw, then a uvarint length that overruns the buffer.
		{"inline_body_len_overruns", []byte{0x01, 0x00, 0x7f}, "short read (bytes)"},
		// Truncate the complete frame one byte before its end so a trailing field
		// (the ContentType str) reads short.
		{"truncated_mid_frame", full[:len(full)-1], "short read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := audit.ReadBodyBinary(tc.data)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestReadBodyBinary_LengthOverflowNoPanic feeds an inline body length prefix
// >= 2^63 (where int(ln) goes negative and would wrap a signed bounds check into a
// make/slice panic) and asserts a clean short-read error instead. A corrupt or
// hostile audit frame must never panic the Hub consume goroutine.
func TestReadBodyBinary_LengthOverflowNoPanic(t *testing.T) {
	// kind=inline (0x01), enc=raw (0x00), then a uvarint length of 2^64-1.
	wire := []byte{0x01, 0x00}
	wire = binary.AppendUvarint(wire, ^uint64(0))
	wire = append(wire, 'a', 'b') // a couple of real bytes follow

	_, _, err := audit.ReadBodyBinary(wire)
	if err == nil {
		t.Fatal("expected short-read error from overflowing inline length, got nil")
	}
	if !strings.Contains(err.Error(), "short read (bytes)") {
		t.Fatalf("error %q does not contain %q", err.Error(), "short read (bytes)")
	}
}

// TestReadBodyBinary_BadUvarint drives the byteReader.uvarint / varint error paths:
// a maximal run of 0x80 continuation bytes never terminates, so Uvarint/Varint
// return n<=0.
func TestReadBodyBinary_BadVarintFields(t *testing.T) {
	// kind=inline, enc=raw, body length 0 (so InlineBytes empty), then a malformed
	// varint for SizeBytes: ten 0x80 continuation bytes overflow uint64.
	bad := []byte{0x01, 0x00, 0x00}
	for range 10 {
		bad = append(bad, 0x80)
	}
	_, _, err := audit.ReadBodyBinary(bad)
	if err == nil {
		t.Fatal("expected error for malformed varint SizeBytes")
	}
	if !strings.Contains(err.Error(), "bad varint") {
		t.Fatalf("error %q does not mention bad varint", err.Error())
	}

	// Malformed uvarint for the inline body length: a lone 0x80 (continuation with
	// no terminator) at the length position.
	badUv := []byte{0x01, 0x00, 0x80}
	_, _, err = audit.ReadBodyBinary(badUv)
	if err == nil {
		t.Fatal("expected error for malformed uvarint body length")
	}
	if !strings.Contains(err.Error(), "bad uvarint") {
		t.Fatalf("error %q does not mention bad uvarint", err.Error())
	}
}

// TestReadBodyBinary_EveryPrefixTruncationErrors walks each field boundary of a
// full inline frame AND a full spill frame: every proper prefix of a valid frame
// must surface a short-read error (never a partial/garbage Body), which exercises
// each per-field `if err != nil { return }` guard in both decode branches.
func TestReadBodyBinary_EveryPrefixTruncationErrors(t *testing.T) {
	frames := map[string][]byte{
		"inline": audit.AppendBodyBinary(nil, mkInline([]byte("café ☕ body"), audit.EncodingText)),
		"spill": audit.AppendBodyBinary(nil, audit.NewSpillBody(&audit.SpillRef{
			Backend: "s3", Key: "k/1", Size: 99, SHA256: "deadbeef", ContentType: "application/json", Truncated: true,
		}, 99, true, "application/json")),
	}
	for name, full := range frames {
		t.Run(name, func(t *testing.T) {
			for cut := 1; cut < len(full); cut++ {
				if _, _, err := audit.ReadBodyBinary(full[:cut]); err == nil {
					t.Fatalf("%s: prefix of length %d (of %d) decoded without error", name, cut, len(full))
				}
			}
			// The complete frame must still decode cleanly (boundary sanity).
			if _, n, err := audit.ReadBodyBinary(full); err != nil || n != len(full) {
				t.Fatalf("%s: full frame failed: err=%v n=%d len=%d", name, err, n, len(full))
			}
		})
	}
}

// TestAppendBodyBinary_NilSpillRefDefaultsEmpty asserts a spill body with a nil
// SpillRef appends a well-formed frame (the writer substitutes an empty ref) that
// reads back to a spill with all-empty ref fields, never a panic.
func TestAppendBodyBinary_NilSpillRefDefaultsEmpty(t *testing.T) {
	in := audit.Body{Kind: audit.BodySpill, SpillRef: nil, SizeBytes: 7, ContentType: "x"}
	frame := audit.AppendBodyBinary(nil, in)
	got, n, err := audit.ReadBodyBinary(frame)
	if err != nil {
		t.Fatalf("ReadBodyBinary: %v", err)
	}
	if n != len(frame) {
		t.Fatalf("consumed %d want %d", n, len(frame))
	}
	if got.Kind != audit.BodySpill || got.SpillRef == nil {
		t.Fatalf("expected spill with non-nil ref, got %+v", got)
	}
	empty := audit.SpillRef{}
	if *got.SpillRef != empty {
		t.Fatalf("nil-ref spill should read back as empty ref, got %+v", *got.SpillRef)
	}
	if got.SizeBytes != 7 || got.ContentType != "x" {
		t.Fatalf("outer fields lost: size=%d ct=%q", got.SizeBytes, got.ContentType)
	}
}
