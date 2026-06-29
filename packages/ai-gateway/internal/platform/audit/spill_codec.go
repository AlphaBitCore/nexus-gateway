package audit

// spill_codec.go — binary-safe framing for the durable NDJSON spill.
//
// The spill stores ALREADY-MARSHALED wire records (see spillData / spillRecord /
// the spillLoop batch). With the binary audit wire (NEXUS_AUDIT_WIRE=binary) a
// record contains arbitrary bytes INCLUDING raw 0x0A, so writing it verbatim into
// the newline-delimited spool and reading it back with ReadBytes('\n') shatters it
// at its internal newlines — the records re-publish as garbage and dead-letter.
// (JSON records escape their newlines, so the legacy path was accidentally safe.)
//
// Fix: every spill line is base64 (newline-free for ANY input), so the spool's
// newline framing stays valid for both wires. The +33% size + encode cost lands
// ONLY on the rare overflow-to-disk path, never steady state. Recovery base64-
// decodes each line back to the original record bytes, then re-frames them for the
// broker using the wire-correct framing (binary = magic + length-prefixed records,
// JSON = NDJSON) so the Hub decodes them exactly as live traffic.

import (
	"encoding/base64"
	"slices"
)

// spillEncodeRecord returns the base64 of one marshaled record — the spool line
// body (the ndjson.Writer appends the '\n' delimiter).
func spillEncodeRecord(rec []byte) []byte {
	out := make([]byte, base64.StdEncoding.EncodedLen(len(rec)))
	base64.StdEncoding.Encode(out, rec)
	return out
}

// appendSpillLine appends base64(rec) + '\n' to dst — the batched spool form used
// by the spillLoop, equivalent to ndjson.Writer.Write but for a pre-assembled block.
func appendSpillLine(dst, rec []byte) []byte {
	n := base64.StdEncoding.EncodedLen(len(rec))
	// slices.Grow gives append-style geometric (amortized O(1)) growth. The prior
	// exact-size make+copy reallocated and copied the WHOLE accumulated block on
	// every line that exceeded cap, making a full spill-flush cycle O(n^2) in bytes
	// copied. Byte output and the base64 NDJSON spool format are byte-identical, so
	// spill recovery is unaffected.
	dst = slices.Grow(dst, n+1)
	end := len(dst)
	dst = dst[:end+n]
	base64.StdEncoding.Encode(dst[end:], rec)
	return append(dst, '\n')
}

// spillDecodeLine decodes one spool line (base64, newline already trimmed) back to
// the original marshaled record bytes. A line that is not valid base64 is returned
// as-is with ok=false so a legacy (pre-base64) raw-JSON spool still drains during
// an upgrade rather than being lost.
func spillDecodeLine(line []byte) (rec []byte, ok bool) {
	dec := make([]byte, base64.StdEncoding.DecodedLen(len(line)))
	n, err := base64.StdEncoding.Decode(dec, line)
	if err != nil {
		return line, false
	}
	return dec[:n], true
}
