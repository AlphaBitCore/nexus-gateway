package consumer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	json "github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// frameAck resolves a single NATS message that carries one OR MORE audit records
// as a newline-delimited NDJSON frame. The gateway batches many records into one
// publish to cut the per-record PublishAsync op-count (the measured audit-drain
// bottleneck); the Hub must therefore ack/nak the underlying message exactly
// once, only AFTER every record it carried has been durably resolved (committed,
// dead-lettered, or poison-skipped).
//
// remaining counts records still pending. The first record to request a nak makes
// the whole frame nak: every record in the frame redelivers, and dedup by request
// id (the traffic_event at-least-once contract) makes the already-committed ones
// idempotent. The done guard makes resolve/forceNak safe to interleave — once the
// message is settled, later resolutions from an in-flight batch are no-ops, so a
// record that commits after a sibling forced a nak simply redelivers and dedupes.
//
// Post-commit ack ordering is preserved: resolve() is only ever called from the
// post-DB-commit ack path (ackAll / ackDeadLettered) or the retry path
// (nakWithBackoff), never before the durable write.
type frameAck struct {
	msg       *mq.Message
	mu        sync.Mutex
	remaining int
	nak       bool
	nakDelay  time.Duration
	done      bool
}

func newFrameAck(msg *mq.Message, records int) *frameAck {
	return &frameAck{msg: msg, remaining: records}
}

// resolve records one record's outcome. nak=true means "this record needs the
// frame redelivered"; nakDelay is its requested backoff (the frame uses the max).
// When the last record resolves, the underlying message is acked once — or nak'd
// once if any record asked to retry.
func (f *frameAck) resolve(nak bool, nakDelay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return
	}
	if nak {
		f.nak = true
		if nakDelay > f.nakDelay {
			f.nakDelay = nakDelay
		}
	}
	if f.remaining > 0 {
		f.remaining--
	}
	if f.remaining > 0 {
		return
	}
	f.settleLocked()
}

// forceNak settles the whole frame as a nak immediately, regardless of how many
// records are still pending. Used when the consumer cannot even hand every record
// to the batch (a synchronous flush failure mid-frame): redelivering the frame and
// letting dedup drop the already-committed records is the only correct recovery.
func (f *frameAck) forceNak() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return
	}
	f.nak = true
	f.settleLocked()
}

// settleLocked performs the single underlying ack/nak. Caller holds f.mu.
func (f *frameAck) settleLocked() {
	f.done = true
	switch {
	case f.nak && f.msg.NakWithDelay != nil && f.nakDelay > 0:
		_ = f.msg.NakWithDelay(f.nakDelay)
	case f.nak:
		_ = f.msg.Nak()
	default:
		_ = f.msg.Ack()
	}
}

// splitFrame splits an NDJSON audit frame into its record lines. A legacy
// single-record message (no interior newline) yields exactly one line, so the
// consumer transparently handles both old per-record producers and new batched
// producers. Records are compact single-line JSON (interior newlines are escaped
// as \n inside JSON strings), so a raw 0x0A only ever appears as a frame
// delimiter; empty segments (a trailing delimiter, blank lines) are skipped.
func splitFrame(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{'\n'})
	out := parts[:0]
	for _, p := range parts {
		if len(bytes.TrimSpace(p)) == 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// isBinaryFrame reports whether data is a binary audit frame (begins with the
// binwire magic). A JSON/NDJSON record begins with '{' (0x7b), never the magic, so
// the Hub can dual-read both forms and drain any in-flight JSON during a rollout.
func isBinaryFrame(data []byte) bool {
	return len(data) > 0 && data[0] == mq.BinwireMagic
}

// splitBinaryFrame splits a binary audit frame (magic byte, then length-prefixed
// records) into its record byte-slices. The slices alias data — the caller copies
// what it retains (decodeBinaryRecord copies every field off the buffer). A
// malformed length yields the records parsed so far rather than a panic; a short
// frame is treated as empty.
func splitBinaryFrame(data []byte) [][]byte {
	if len(data) < 1 || data[0] != mq.BinwireMagic {
		return nil
	}
	var out [][]byte
	pos := 1
	for pos < len(data) {
		ln, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			break // malformed length prefix — stop at what we have
		}
		pos += n
		// Unsigned bound: a length >= 2^63 makes int(ln) negative and wraps the
		// check, then panics the slice. Compare remaining bytes as uint64 so a
		// corrupt length truncates cleanly instead of crashing the Hub.
		if ln > uint64(len(data)-pos) {
			break // truncated or corrupt record length
		}
		out = append(out, data[pos:pos+int(ln)])
		pos += int(ln)
	}
	return out
}

// alertViewPool reuses the decode target across records so the high-volume,
// read-only alerts drain never allocates an AlertView per event.
var alertViewPool = sync.Pool{New: func() any { return new(AlertView) }}

// DecodeAlertFrame decodes one audit frame (binary TLV or legacy NDJSON-of-JSON)
// for read-only consumers (the alerts engine), invoking onRecord once per decoded
// record and onError once per record that fails to decode.
//
// Performance contract — this is the hot alert-drain path:
//   - The *AlertView passed to onRecord is POOLED and valid ONLY for the duration
//     of the call. onRecord MUST read what it needs synchronously and MUST NOT
//     retain the pointer (the alerts aggregators copy scalar values into their
//     windows, so this holds).
//   - Only the 22 fields the aggregators read are decoded; the other 80 producer
//     fields (including kilobyte-scale normalized json) are skip-advanced, never
//     copied. Bodies are decoded metadata-only (Kind/Truncated/…): the inline
//     content bytes are skipped via their length prefix, never copied.
//   - The decode target is pooled, so a steady-state frame allocates no per-record
//     struct.
//
// The DB writer keeps its own full-body decode into TrafficEventMessage (it
// persists every column + the payload); this path deliberately does not.
func DecodeAlertFrame(data []byte, onRecord func(*AlertView), onError func(error)) {
	binaryFrame := isBinaryFrame(data)
	var lines [][]byte
	if binaryFrame {
		lines = splitBinaryFrame(data)
	} else {
		lines = splitFrame(data)
	}
	for _, line := range lines {
		v := alertViewPool.Get().(*AlertView)
		*v = AlertView{} // reset: drop the prior record before reuse
		var derr error
		if binaryFrame {
			derr = decodeBinaryRecordIntoViewSafe(v, line)
		} else {
			// Legacy NDJSON: unmarshal straight into the narrow view; json ignores
			// the producer keys that have no AlertView field.
			derr = json.Unmarshal(line, v)
		}
		if derr != nil {
			if onError != nil {
				onError(derr)
			}
			alertViewPool.Put(v)
			continue
		}
		onRecord(v)
		alertViewPool.Put(v)
	}
}

// decodeBinaryRecordIntoViewSafe is the panic-guarded, body-meta (zero-copy)
// narrow decode for the alert path — a corrupt or hostile frame becomes an error,
// never a Hub crash-loop via JetStream redelivery.
func decodeBinaryRecordIntoViewSafe(v *AlertView, data []byte) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("binwire: decode panic: %v", rec)
		}
	}()
	return decodeBinaryRecordIntoView(v, data)
}
