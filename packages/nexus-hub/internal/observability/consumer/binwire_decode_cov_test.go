package consumer

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// rawField appends a field-id (uvarint) to a record builder; the caller appends
// the payload bytes for that field's wire type. This lets the tests below hand-
// craft records that are valid up to a deliberately truncated payload, so each
// recReader method's short-read / bad-decode error branch is exercised against a
// real decode (not a synthetic recReader poke).
func rawField(id mq.FieldID) []byte {
	return binary.AppendUvarint(nil, uint64(id))
}

// TestDecodeBinaryRecordShortReads feeds decodeBinaryRecord records whose payload
// for one field is truncated, and asserts the specific binwire error each reader
// raises. Each case isolates one wire type: varint, f64, bool, bytes-length, and
// the bytes body after a good length. These are the error branches the happy-path
// round-trip tests never reach.
func TestDecodeBinaryRecordShortReads(t *testing.T) {
	cases := []struct {
		name    string
		record  []byte
		wantErr string
	}{
		{
			// FldLatencyMs is a varint. A 0x80 continuation byte with no terminating
			// byte is an incomplete varint → binary.Varint returns n<=0.
			name:    "bad_varint",
			record:  append(rawField(mq.FldLatencyMs), 0x80),
			wantErr: "binwire: bad varint",
		},
		{
			// FldReasoningCostUsd is an 8-byte little-endian f64. Provide only 3 of 8
			// payload bytes → short read (f64).
			name:    "short_f64",
			record:  append(rawField(mq.FldReasoningCostUsd), 0x01, 0x02, 0x03),
			wantErr: "binwire: short read (f64)",
		},
		{
			// FldAttestationVerified is a single bool byte. Provide the field-id with
			// no payload → short read (bool).
			name:    "short_bool",
			record:  rawField(mq.FldAttestationVerified),
			wantErr: "binwire: short read (bool)",
		},
		{
			// FldID is a length-prefixed string. A 0x80 continuation byte for the
			// length is an incomplete uvarint → bad uvarint.
			name:    "bad_bytes_length_uvarint",
			record:  append(rawField(mq.FldID), 0x80),
			wantErr: "binwire: bad uvarint",
		},
		{
			// FldID with a declared length of 5 but only 2 payload bytes present →
			// short read (bytes).
			name: "short_bytes_payload",
			record: func() []byte {
				rec := rawField(mq.FldID)
				rec = binary.AppendUvarint(rec, 5)
				return append(rec, 'a', 'b')
			}(),
			wantErr: "binwire: short read (bytes)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeBinaryRecord(tc.record)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDecodeBinaryRecordStrSliceTruncatedElement targets the sticky-error early
// returns inside the bytesCopy/uvarint path: a strSlice declares 3 elements but
// the second element's length-prefix is truncated. The first element decodes, the
// second sets r.err mid-loop, and the loop's `r.err == nil` guard plus each
// reader's leading `if r.err != nil` early-return short-circuit the rest. The
// observable outcome is the specific bad-uvarint error.
func TestDecodeBinaryRecordStrSliceTruncatedElement(t *testing.T) {
	// FldComplianceTags is a string slice: [count][len][bytes][len][bytes]...
	rec := rawField(mq.FldComplianceTags)
	rec = binary.AppendUvarint(rec, 3) // claim 3 elements
	// element 0: "ok"
	rec = binary.AppendUvarint(rec, 2)
	rec = append(rec, 'o', 'k')
	// element 1: truncated length (continuation byte, no terminator)
	rec = append(rec, 0x80)

	_, err := decodeBinaryRecord(rec)
	if err == nil {
		t.Fatal("expected bad uvarint error from truncated slice element, got nil")
	}
	if !strings.Contains(err.Error(), "binwire: bad uvarint") {
		t.Fatalf("error = %q, want bad uvarint", err.Error())
	}
}

// TestDecodeBinaryRecordStrSliceShortElementBytes covers the strSlice element
// loop where a declared element length exceeds the remaining bytes — the second
// str() call inside the loop raises short read (bytes), and the loop exits via
// its `r.err == nil` condition rather than reading garbage.
func TestDecodeBinaryRecordStrSliceShortElementBytes(t *testing.T) {
	rec := rawField(mq.FldPassthroughFlags) // also a string slice
	rec = binary.AppendUvarint(rec, 2)      // claim 2 elements
	rec = binary.AppendUvarint(rec, 1)      // element 0 length 1
	rec = append(rec, 'x')
	rec = binary.AppendUvarint(rec, 9) // element 1 claims 9 bytes
	rec = append(rec, 'y', 'z')        // only 2 present

	_, err := decodeBinaryRecord(rec)
	if err == nil {
		t.Fatal("expected short read (bytes) from oversized slice element, got nil")
	}
	if !strings.Contains(err.Error(), "binwire: short read (bytes)") {
		t.Fatalf("error = %q, want short read (bytes)", err.Error())
	}
}

// TestDecodeBinaryRecordBadFieldID exercises the leading-uvarint failure at the
// loop head (the field-id itself is a truncated uvarint) and the body() error
// branch via a malformed body envelope on FldRequestBody.
func TestDecodeBinaryRecordBadFieldID(t *testing.T) {
	// A lone 0x80 is an incomplete uvarint for the field-id at the loop head.
	if _, err := decodeBinaryRecord([]byte{0x80}); err == nil ||
		!strings.Contains(err.Error(), "binwire: bad uvarint") {
		t.Fatalf("bad field-id uvarint: got err=%v, want bad uvarint", err)
	}
}

// TestDecodeBinaryRecordBodyShortRead drives the body() error branch: FldRequestBody
// expects an audit body envelope; a single stray byte is not a valid envelope, so
// audit.ReadBodyBinary returns an error that the reader makes sticky.
func TestDecodeBinaryRecordBodyShortRead(t *testing.T) {
	rec := append(rawField(mq.FldRequestBody), 0x07) // not a valid body envelope
	if _, err := decodeBinaryRecord(rec); err == nil {
		t.Fatal("expected error from malformed body envelope, got nil")
	}
}

// TestDecodeBinaryRecordLengthOverflowNoPanic feeds length/count prefixes >= 2^63
// (where int(ln) would go negative and wrap a signed bounds check) and asserts the
// decoder returns a clean short-read error instead of panicking. A hostile or
// corrupt frame on the audit subject must never crash the Hub's consume goroutine.
func TestDecodeBinaryRecordLengthOverflowNoPanic(t *testing.T) {
	maxU := ^uint64(0) // 2^64-1, high bit set → negative as int

	t.Run("bytes_length_overflow", func(t *testing.T) {
		rec := rawField(mq.FldID)
		rec = binary.AppendUvarint(rec, maxU) // claim ~18 EiB of string bytes
		rec = append(rec, 'a', 'b')
		_, err := decodeBinaryRecord(rec)
		if err == nil || !strings.Contains(err.Error(), "binwire: short read (bytes)") {
			t.Fatalf("got err=%v, want short read (bytes)", err)
		}
	})

	t.Run("strslice_count_overflow_no_huge_alloc", func(t *testing.T) {
		rec := rawField(mq.FldComplianceTags)
		rec = binary.AppendUvarint(rec, maxU) // lie: ~18e18 elements
		// No element bytes follow, so the first element read short-circuits. The
		// pre-allocation is capped to the remaining bytes, so this neither hangs
		// nor allocates a giant slice.
		_, err := decodeBinaryRecord(rec)
		if err == nil {
			t.Fatal("expected error from overflowing element count, got nil")
		}
	})
}

// TestDecodeBinaryRecordSafeRecoversBodyOverflow drives the defense-in-depth
// recover wrapper: a body envelope whose inner length prefix lies (>= 2^63) must
// be converted to a decode error, never a panic that crashes the consume
// goroutine and crash-loops the Hub via JetStream redelivery.
func TestDecodeBinaryRecordSafeRecoversBodyOverflow(t *testing.T) {
	// Construct a FldRequestBody whose audit body envelope declares an overflowing
	// inline length. ReadBodyBinary's own bound is hardened separately; the wrapper
	// guarantees that even an unhardened panic on this path is contained.
	rec := rawField(mq.FldRequestBody)
	// audit.Body binary envelope: a leading codec/flag byte then a uvarint length.
	// A lying length prefix is the overflow vector; whatever the inner layout, the
	// wrapper must return an error rather than propagate a panic.
	rec = append(rec, 0x00)
	rec = binary.AppendUvarint(rec, ^uint64(0))

	evt, err := decodeBinaryRecordSafe(rec)
	if err == nil {
		t.Fatalf("expected decode error (recovered or returned), got nil; evt=%+v", evt)
	}
}

// TestDecodeBinaryRecordEmptyOpaqueAndSlice covers the zero-length short-circuits
// that the populated round-trip cases skip: an opaque-JSON field (FldIdentity)
// encoded as a zero-length byte run decodes to a nil RawMessage (not an empty
// non-nil slice), and a string-slice field (FldComplianceTags) with a declared
// count of 0 decodes to a nil slice. Both must persist as SQL NULL, so the
// decoded pointers must be nil.
func TestDecodeBinaryRecordEmptyOpaqueAndSlice(t *testing.T) {
	// FldIdentity (json) with a length-prefix of 0 → json() returns nil.
	rec := rawField(mq.FldIdentity)
	rec = binary.AppendUvarint(rec, 0)
	// FldComplianceTags (strSlice) with count 0 → strSlice() returns nil.
	rec = append(rec, rawField(mq.FldComplianceTags)...)
	rec = binary.AppendUvarint(rec, 0)
	// FldEndpointType (a non-pointer str() field) with a length-prefix of 0 → str()
	// hits its ln==0 short-circuit (returns "" after advancing r.n by the varint
	// only). Locks the H5 single-copy str()'s empty-path r.n advancement.
	rec = append(rec, rawField(mq.FldEndpointType)...)
	rec = binary.AppendUvarint(rec, 0)
	// A trailing real scalar proves the stream stayed in sync past the empties.
	rec = append(rec, rawField(mq.FldID)...)
	rec = binary.AppendUvarint(rec, 3)
	rec = append(rec, 'e', 'v', 't')

	got, err := decodeBinaryRecord(rec)
	if err != nil {
		t.Fatalf("decode of empty opaque/slice fields: %v", err)
	}
	if got.Identity != nil {
		t.Fatalf("zero-length Identity decoded to %v, want nil (SQL NULL)", got.Identity)
	}
	if got.ComplianceTags != nil {
		t.Fatalf("zero-count ComplianceTags decoded to %v, want nil", got.ComplianceTags)
	}
	if got.EndpointType != "" {
		t.Fatalf("zero-length EndpointType decoded to %q, want empty (str() ln==0 path)", got.EndpointType)
	}
	if got.ID != "evt" {
		t.Fatalf("trailing ID = %q, want \"evt\" (stream desynced past empty fields)", got.ID)
	}
}
