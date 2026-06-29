package audit

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/goccy/go-json"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// frameCapProducer records every EnqueueBatchAsync call so a test can assert how
// many NATS messages (publishes) a flush produced and what they carried.
type frameCapProducer struct {
	mu        sync.Mutex
	published [][][]byte // one entry per EnqueueBatchAsync call
}

func (p *frameCapProducer) Publish(context.Context, string, []byte) error { return nil }
func (p *frameCapProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (p *frameCapProducer) Close() error                                  { return nil }

func (p *frameCapProducer) EnqueueBatchAsync(_ context.Context, _ string, batch [][]byte) ([]error, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([][]byte, len(batch))
	for i, b := range batch {
		cp[i] = append([]byte(nil), b...)
	}
	p.published = append(p.published, cp)
	errs := make([]error, len(batch))
	return errs, nil
}

func (p *frameCapProducer) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

// recordsPublished counts the records across every published frame, honouring the
// active wire (binary magic+length-prefixed, or JSON newline-delimited NDJSON).
func (p *frameCapProducer) recordsPublished() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, call := range p.published {
		for _, frame := range call {
			n += countFrameRecords(frame)
		}
	}
	return n
}

// countFrameRecords counts the records carried by one published frame. A binary
// frame opens with mq.BinwireMagic and length-prefixes each record (uvarint), since
// binary bodies may contain any byte including '\n'; a JSON frame is newline-
// delimited NDJSON. Mirrors the Hub's splitFrame so the test counts exactly what the
// consumer would.
func countFrameRecords(frame []byte) int {
	if len(frame) > 0 && frame[0] == mq.BinwireMagic {
		n, i := 0, 1
		for i < len(frame) {
			l, w := binary.Uvarint(frame[i:])
			if w <= 0 || i+w+int(l) > len(frame) {
				break
			}
			i += w + int(l)
			n++
		}
		return n
	}
	return len(splitLines(frame))
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, line)
		}
	}
	return out
}

func newFrameTestRecord(id string, bodyKB int) *Record {
	body := bytes.Repeat([]byte(`{"k":"vvvvvvvvvvvvvvvv"}`), bodyKB*1024/24)
	return &Record{RequestID: id, Timestamp: time.Unix(1700000000, 0).UTC(), RequestBody: body, StatusCode: 200, Path: "/v1/x"}
}

// With framing enabled, many records collapse into ONE publish per frame (the
// op-count win), and every record is still carried (no loss). Runs on the binary
// wire (the production default) and counts via the magic+length-prefixed framing.
func TestPublishFramed_PacksManyRecordsIntoFewMessages(t *testing.T) {
	t.Setenv("NEXUS_AUDIT_WIRE", "binary")
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()).WithFramePublish(256 * 1024) // ~256 KB frames

	// 40 records of ~16 KB each ≈ 640 KB total → ~3 frames of 256 KB, NOT 40 publishes.
	batch := make([]*Record, 40)
	for i := range batch {
		batch[i] = newFrameTestRecord(string(rune('a'+i%26))+string(rune('0'+i/26)), 16)
	}
	w.publishBatchOn(0, batch)

	if got := prod.recordsPublished(); got != 40 {
		t.Fatalf("records published = %d, want 40 (every record must ship)", got)
	}
	if calls := prod.calls(); calls >= 40 || calls == 0 {
		t.Fatalf("publish calls = %d, want far fewer than 40 (frames), >0", calls)
	}
	t.Logf("40 records shipped in %d frame publishes (op-count cut %.0fx)", prod.calls(), 40.0/float64(prod.calls()))
}

// A single record larger than the frame cap still ships (in its own frame) — a
// frame is never empty and a big record is never dropped. Binary wire (production
// default).
func TestPublishFramed_OversizeRecordShipsAlone(t *testing.T) {
	t.Setenv("NEXUS_AUDIT_WIRE", "binary")
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()).WithFramePublish(8 * 1024) // 8 KB cap

	batch := []*Record{newFrameTestRecord("big", 64)} // 64 KB record >> 8 KB cap
	w.publishBatchOn(0, batch)

	if got := prod.recordsPublished(); got != 1 {
		t.Fatalf("records published = %d, want 1 (oversize record must still ship)", got)
	}
}

// Legacy mode (no WithFramePublish) keeps one publish per record.
func TestPublishFramed_DisabledKeepsPerRecord(t *testing.T) {
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()) // framing OFF

	batch := []*Record{newFrameTestRecord("a", 4), newFrameTestRecord("b", 4), newFrameTestRecord("c", 4)}
	w.publishBatchOn(0, batch)

	if got := prod.recordsPublished(); got != 3 {
		t.Fatalf("records published = %d, want 3", got)
	}
}

// Every marshaled record is a single line (no interior raw newline) so NDJSON
// framing is unambiguous — the load-bearing invariant for the Hub's splitFrame on
// the JSON wire (the binary wire is magic+length-prefixed, so this invariant is
// JSON-only; binary framing is covered by the Hub-consumer binwire tests).
func TestMarshalRecord_IsSingleLine(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	// A body whose JSON contains an escaped newline inside a string value.
	rec := &Record{
		RequestID:   "nl",
		Timestamp:   time.Unix(1700000000, 0).UTC(),
		RequestBody: []byte(`{"text":"line1\nline2","n":1}`),
		StatusCode:  200,
		Path:        "/v1/x",
	}
	data, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord returned !ok")
	}
	if bytes.IndexByte(data, '\n') != -1 {
		t.Fatalf("marshaled record contains a raw newline — NDJSON framing would mis-split it:\n%s", data)
	}
}

// TestFlushBatchAsync_BufferHoldRoundTripsAcrossReuse guards the marshalRecord
// buffer-hold: marshalRecord returns bytes that ALIAS a pooled buffer, reclaimed
// only after publish. Flushing twice forces the second batch to reuse buffers the
// first batch returned to the pool — a reclaim-too-early or missing-reset bug
// would corrupt the second round's published bytes. We decode every published
// record and assert the identity fields survive both rounds.
func TestFlushBatchAsync_BufferHoldRoundTripsAcrossReuse(t *testing.T) {
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()) // framing OFF → non-framed buffer-hold path
	mk := func(id, model string) *Record {
		return &Record{
			RequestID: id, Timestamp: time.Unix(1700000000, 0).UTC(),
			RequestBody: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`),
			ModelName:   model, StatusCode: 200, Path: "/v1/x",
		}
	}
	for round := range 2 {
		prod.mu.Lock()
		prod.published = nil
		prod.mu.Unlock()
		idA, idB := "a"+strconv.Itoa(round), "b"+strconv.Itoa(round)
		w.publishBatchOn(0, []*Record{mk(idA, "m1"), mk(idB, "m2")})

		seen := map[string]string{}
		prod.mu.Lock()
		calls := prod.published
		prod.mu.Unlock()
		for _, call := range calls {
			for _, payload := range call {
				for _, line := range splitLines(payload) {
					var msg mq.TrafficEventMessage
					if err := json.Unmarshal(line, &msg); err != nil {
						t.Fatalf("round %d: published bytes do not decode: %v\nbytes=%s", round, err, line)
					}
					seen[msg.ID] = msg.ModelName
				}
			}
		}
		if seen[idA] != "m1" || seen[idB] != "m2" {
			t.Fatalf("round %d: decoded records corrupted (buffer-hold reuse bug): %v", round, seen)
		}
	}
}

// Reusing a pooled frame buffer across publishes must not leak stale bytes: a
// large first batch then a small second batch through the same Writer. Every
// record the second flush publishes must be a second-batch record with valid
// JSON, and no first-batch id may appear in the second flush's frames — a pool
// buffer that wasn't length-reset on reuse would prepend batch-A bytes.
func TestPublishFramed_PooledFrameReuse_NoStaleBytes(t *testing.T) {
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()).WithFramePublish(256 * 1024)

	// Batch A: many large records — fills and grows the pooled frame buffers.
	batchA := make([]*Record, 30)
	for i := range batchA {
		batchA[i] = newFrameTestRecord("A"+strconv.Itoa(i), 16)
	}
	w.publishBatchOn(0, batchA)
	callsAfterA := prod.calls()

	// Batch B: small records with distinct ids — reuses the now-pooled buffers.
	batchB := make([]*Record, 8)
	for i := range batchB {
		batchB[i] = newFrameTestRecord("B"+strconv.Itoa(i), 1)
	}
	w.publishBatchOn(0, batchB)

	prod.mu.Lock()
	bCalls := append([][][]byte(nil), prod.published[callsAfterA:]...)
	prod.mu.Unlock()
	seen := map[string]bool{}
	for _, call := range bCalls {
		for _, frame := range call {
			for _, line := range splitLines(frame) {
				var msg mq.TrafficEventMessage
				if err := json.Unmarshal(line, &msg); err != nil {
					t.Fatalf("batch-B frame line not valid JSON (stale-byte contamination): %v", err)
				}
				if len(msg.ID) > 0 && msg.ID[0] == 'A' {
					t.Fatalf("batch-A record %q leaked into a batch-B frame (pooled-buffer stale bytes)", msg.ID)
				}
				seen[msg.ID] = true
			}
		}
	}
	if len(seen) != len(batchB) {
		t.Fatalf("batch-B published %d distinct records, want %d", len(seen), len(batchB))
	}
}
