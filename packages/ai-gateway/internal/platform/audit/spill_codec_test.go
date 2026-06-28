package audit

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// TestAppendSpillLine_ManyRecordsByteIdentical stresses the geometric-growth path
// (slices.Grow): appending many variable-length records — including ones with raw
// 0x0A — must produce a block byte-identical to base64(rec)+'\n' per line, and
// every record must decode back byte-for-byte in order. This locks the
// O(n^2)->amortized growth change as output-preserving.
func TestAppendSpillLine_ManyRecordsByteIdentical(t *testing.T) {
	var recs [][]byte
	for i := range 200 {
		r := make([]byte, (i*37)%1024) // 0..1023 bytes, varied
		for j := range r {
			r[j] = byte((i*7 + j*13) % 256) // includes 0x0A and full binary range
		}
		recs = append(recs, r)
	}

	var buf, want []byte
	for _, r := range recs {
		buf = appendSpillLine(buf, r)
		want = append(want, base64.StdEncoding.AppendEncode(nil, r)...)
		want = append(want, '\n')
	}
	if !bytes.Equal(buf, want) {
		t.Fatalf("appendSpillLine block differs from base64+newline reference (len %d vs %d)", len(buf), len(want))
	}

	lines := bytes.Split(bytes.TrimSuffix(buf, []byte{'\n'}), []byte{'\n'})
	if len(lines) != len(recs) {
		t.Fatalf("split yielded %d lines, want %d", len(lines), len(recs))
	}
	for i, l := range lines {
		dec, ok := spillDecodeLine(l)
		if !ok || !bytes.Equal(dec, recs[i]) {
			t.Fatalf("record %d did not round-trip (ok=%v, %d bytes vs %d)", i, ok, len(dec), len(recs[i]))
		}
	}

	// Production pattern: the spillLoop reuses one buffer across flushes via
	// buf = buf[:0] (cap retained). With slices.Grow the retained cap means later
	// flushes usually realloc zero times; assert each flush's block is still
	// byte-identical so a future "append to stale tail" refactor can't slip in.
	buf = buf[:0]
	for f := range 5 {
		buf = buf[:0]
		var ref []byte
		for _, r := range recs[:20] {
			buf = appendSpillLine(buf, r)
			ref = append(ref, base64.StdEncoding.AppendEncode(nil, r)...)
			ref = append(ref, '\n')
		}
		if !bytes.Equal(buf, ref) {
			t.Fatalf("flush %d (cap-reuse) block differs from reference", f)
		}
	}
}

// TestSpillCodec_BinaryRecordSurvivesNewlineSpool locks the binary-wire spill bug:
// a binary record containing raw 0x0A bytes, written as a base64 spool line, must
// NOT shatter when the spool is split on '\n', and must decode back byte-for-byte.
// Writing the record verbatim (the old behaviour) split it at its internal newlines
// → fragments → dead-letter.
func TestSpillCodec_BinaryRecordSurvivesNewlineSpool(t *testing.T) {
	rec := []byte{0x01, 0x0a, 0xff, 0x00, 0x0a, 0x7b, 0x9f, 0x31, 0x0a, 0x6e} // raw 0x0A inside
	line := spillEncodeRecord(rec)
	if bytes.IndexByte(line, '\n') >= 0 {
		t.Fatal("base64 spool line must be newline-free")
	}
	// Simulate the spool: line + '\n', then the recovery's split-on-newline.
	spool := append(append([]byte(nil), line...), '\n')
	parts := bytes.Split(bytes.TrimRight(spool, "\n"), []byte{'\n'})
	if len(parts) != 1 {
		t.Fatalf("binary record shattered into %d spool lines", len(parts))
	}
	got, ok := spillDecodeLine(parts[0])
	if !ok || !bytes.Equal(got, rec) {
		t.Fatalf("recovered record mismatch: ok=%v got=%x want=%x", ok, got, rec)
	}

	// appendSpillLine (batched form) must produce the same newline-safe framing.
	buf := appendSpillLine(appendSpillLine(nil, rec), rec)
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("batched spool: 2 records → %d lines", len(lines))
	}
	for i, l := range lines {
		dec, ok := spillDecodeLine(l)
		if !ok || !bytes.Equal(dec, rec) {
			t.Fatalf("batched line %d mismatch", i)
		}
	}

	// Legacy raw-JSON spool lines (pre-base64 upgrade) still drain: a non-base64
	// line is returned verbatim so an in-flight spool is never lost on upgrade.
	legacy := []byte(`{"id":"legacy"}`)
	if dec, ok := spillDecodeLine(legacy); ok || !bytes.Equal(dec, legacy) {
		t.Fatalf("legacy raw line must pass through verbatim, got ok=%v %s", ok, dec)
	}
}
