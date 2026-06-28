// writer_batch.go — the audit Writer's async-batch publish path: the pooled
// per-record marshal (parallel across the chunk), chunked JetStream publish,
// and per-record failure routing. Split from writer.go, which owns the Writer
// lifecycle, buffer admission, and flush loop.
package audit

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"

	// goccy/go-json is the standard JSON library across the codebase. Here it
	// replaces encoding/json on the hot audit marshal: measured ~3x faster than
	// stdlib on the real TrafficEventMessage (308µs -> 99µs/record) because its
	// encode path carries far less reflection overhead. The wire envelope needs
	// no byte-identity (the Hub Unmarshals it), so a faster encoder that decodes
	// to the same data is a pure throughput win on this CPU-bound side-path.
	json "github.com/goccy/go-json"
)

// msgBufPool reuses the per-record marshal buffer for the TrafficEventMessage
// envelope. json.Marshal of the envelope (with the large request/response
// normalized fields embedded) was the single biggest heap allocator on the
// audit path; encoding into a pooled buffer reuses the backing array instead
// of allocating a fresh ~body-sized slice per record. Byte-identity of the
// envelope is NOT required — the Hub consumer json.Unmarshals it (tolerating
// the trailing newline json.Encoder appends); only request_normalized needs
// byte-identity and that is produced upstream by normalize, unchanged here.
// msgBufPool buffers are pre-grown to a typical captured-body size so the first
// Encode of a large payload does not pay incremental growSlice churn (measured
// ~19% of flush CPU). Reset() keeps the underlying capacity, so the pre-grow is
// amortized across all reuses of a pooled buffer.
var msgBufPool = sync.Pool{New: func() any {
	b := new(bytes.Buffer)
	b.Grow(64 << 10)
	return b
}}

// msgBufReclaimCap bounds the capacity a marshal buffer may have and still be
// returned to the pool — one oversized record must not inflate every pooled
// buffer thereafter.
const msgBufReclaimCap = 4 << 20 // 4 MiB

// reclaimMsgBuf returns a marshal buffer to the pool once its bytes are no longer
// referenced (the producer has taken them). Buffers that ballooned past the cap
// are dropped to GC instead of pooled.
func reclaimMsgBuf(buf *bytes.Buffer) {
	if buf != nil && buf.Cap() <= msgBufReclaimCap {
		msgBufPool.Put(buf)
	}
}

// reclaimMsgBufs returns a whole chunk's marshal buffers to the pool.
func reclaimMsgBufs(bufs []*bytes.Buffer) {
	for _, b := range bufs {
		reclaimMsgBuf(b)
	}
}

// framePool reuses the NDJSON frame buffer publishFramed hands to the async batch
// publish. Each frame packs many record bytes + '\n' delimiters and was allocated
// exact-size-fresh per publish — the single largest controllable audit allocation
// under load (~10 GB/window). Like the per-record msgBuf, the frame bytes are taken
// by EnqueueBatchAsync (which waits for the broker ack before returning, so the
// payload is fully copied to the wire — the same buffer-hold guarantee the
// non-framed path's reclaimMsgBufs already relies on), after which the buffer
// returns here for reuse. Pre-grown to a typical frame size so packing a chunk
// does not pay append-growth churn.
var framePool = sync.Pool{New: func() any { b := make([]byte, 0, 256<<10); return &b }}

// frameReclaimCap bounds a frame buffer's capacity for pool return — a single
// oversized frame (a record larger than frameMaxBytes, shipped alone) must not
// inflate every pooled buffer thereafter.
const frameReclaimCap = 8 << 20 // 8 MiB

// reclaimFrames returns a publish's frame buffers to the pool once EnqueueBatchAsync
// has taken their bytes. Buffers that ballooned past the cap are dropped to GC.
func reclaimFrames(handles []*[]byte) {
	for _, hp := range handles {
		if hp != nil && cap(*hp) <= frameReclaimCap {
			framePool.Put(hp)
		}
	}
}

// acquireFrame returns a pooled frame buffer truncated to zero length with room for
// size bytes, plus its handle for reclaim. A pooled buffer too small for this frame
// is replaced with an exact-size allocation (still reclaimable if under the cap).
func acquireFrame(size int) (frame []byte, handle *[]byte) {
	hp := framePool.Get().(*[]byte)
	frame = (*hp)[:0]
	if cap(frame) < size {
		frame = make([]byte, 0, size)
	}
	return frame, hp
}

// defaultBinaryFrameBytes is the frame cap applied when the binary wire is enabled
// without an explicit NEXUS_AUDIT_FRAME_MAX_BYTES — large enough to batch many small
// records into one NATS message yet well under the default 1 MiB NATS max_payload so
// a frame never overflows the broker. Binary always frames (see ensureStarted).
const defaultBinaryFrameBytes = 256 << 10

// recordFrameOverhead is the per-record framing cost used to size a frame exactly:
// the JSON path adds one '\n' delimiter; the binary path adds the uvarint length
// prefix that delimits each length-prefixed record under the frame magic.
func recordFrameOverhead(binaryWire bool, recLen int) int {
	if !binaryWire {
		return 1 // '\n' delimiter
	}
	return uvarintLen(uint64(recLen))
}

// uvarintLen returns the number of bytes binary.AppendUvarint will emit for v.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// perfNoPublish is a THROWAWAY perf-ablation switch (NEXUS_PERF_NO_PUBLISH=1):
// the async batch path marshals every record (incurring marshal CPU + alloc)
// but skips the NATS publish, so the audit queue drains instantly. Used ONLY to
// separate the marshal cost from the NATS-publish-driven queue-backup heap.
// Never set in production. Remove with the other perf guards.
var perfNoPublish = os.Getenv("NEXUS_PERF_NO_PUBLISH") == "1"

// batchProducer is the OPTIONAL async-batch capability beyond mq.Producer. The
// NATS producer implements it (EnqueueBatchAsync); the writer type-asserts for
// it and falls back to the per-record Enqueue path when absent (tests, other
// transports). Kept out of the mq.Producer interface so adding it does not
// break that shipped contract's other implementers.
type batchProducer interface {
	EnqueueBatchAsync(ctx context.Context, queue string, batch [][]byte) ([]error, error)
}

// pooledBatchProducer is the OPTIONAL connection-pool capability beyond
// batchProducer. The NATS producer implements it (PoolSize + EnqueueBatchAsyncOn);
// when present, flushBatchAsync runs one publish worker per pool connection so the
// per-connection ack barriers pipeline concurrently instead of serialising on a
// single flush loop (the measured ~1.3k rec/s single-connection ceiling). Producers
// without it (tests, other transports) keep the single-connection path.
type pooledBatchProducer interface {
	PoolSize() int
	EnqueueBatchAsyncOn(ctx context.Context, queue string, batch [][]byte, connIdx int) ([]error, error)
}

// batchPublish publishes batch on the given pool connection when the producer is
// pooled, else on its single connection. Returns the per-message error slice.
func (w *Writer) batchPublish(bp batchProducer, connIdx int, ctx context.Context, batch [][]byte) ([]error, error) {
	if pooled, ok := bp.(pooledBatchProducer); ok {
		return pooled.EnqueueBatchAsyncOn(ctx, w.queue, batch, connIdx)
	}
	return bp.EnqueueBatchAsync(ctx, w.queue, batch)
}

const (
	// batchPublishTimeout bounds how long ONE chunk waits for its async batch to
	// be acked before treating still-unresolved records as failed
	// (re-buffer/spill). Kept under closeShutdownDeadline so Close's drainBuffer
	// loop (which checks the wall only between flushes) cannot overrun even
	// across two chunk flushes.
	batchPublishTimeout = 5 * time.Second
)

// batchMaxCount / batchMaxBytes bound how much of a (possibly very large,
// backed-up) batch is MARSHALED and held in flight at once. Without this the
// whole batch is materialized into byte slices simultaneously — measured as a
// ~2x heap regression vs the per-record path. Chunking caps the live marshaled
// bytes to one chunk (count OR bytes, whichever trips first). Vars (not consts)
// so tests can drive the chunk boundary without allocating 64 MiB of bodies.
var (
	batchMaxCount = 512
	batchMaxBytes = 64 << 20 // 64 MiB
)

// marshalChunkSerial marshals a chunk's records on the CALLING goroutine (one core),
// returning the successfully-marshaled bytes + their backing buffers + records in
// input order. Used by the per-shard publish workers, where the parallelism is the
// N shards running concurrently rather than a parallel marshal within each.
func (w *Writer) marshalChunkSerial(chunk []*Record) ([][]byte, []*bytes.Buffer, []*Record) {
	outD := make([][]byte, 0, len(chunk))
	outB := make([]*bytes.Buffer, 0, len(chunk))
	outR := make([]*Record, 0, len(chunk))
	for _, rec := range chunk {
		if d, b, ok := w.marshalRecord(rec); ok {
			outD = append(outD, d)
			outB = append(outB, b)
			outR = append(outR, rec)
		}
	}
	return outD, outB, outR
}

// marshalChunkParallel marshals the chunk's records concurrently across
// GOMAXPROCS workers, returning the successfully-marshaled bytes and their
// records in input order. Order is immaterial to the Hub (it dedupes by id) but
// preserving it keeps publish batches deterministic. A record that fails to
// marshal is omitted (logged in marshalRecord).
func (w *Writer) marshalChunkParallel(chunk []*Record) ([][]byte, []*bytes.Buffer, []*Record) {
	n := len(chunk)
	if n == 0 {
		return nil, nil, nil
	}
	if n == 1 {
		if d, b, ok := w.marshalRecord(chunk[0]); ok {
			return [][]byte{d}, []*bytes.Buffer{b}, []*Record{chunk[0]}
		}
		return nil, nil, nil
	}
	datas := make([][]byte, n)
	bufs := make([]*bytes.Buffer, n)
	oks := make([]bool, n)
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	var next atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1))
				if i >= n {
					return
				}
				datas[i], bufs[i], oks[i] = w.marshalRecord(chunk[i])
			}
		}()
	}
	wg.Wait()
	outD := make([][]byte, 0, n)
	outB := make([]*bytes.Buffer, 0, n)
	outR := make([]*Record, 0, n)
	for i := range n {
		if oks[i] {
			outD = append(outD, datas[i])
			outB = append(outB, bufs[i])
			outR = append(outR, chunk[i])
		}
	}
	return outD, outB, outR
}

// publishFramed packs the chunk's marshaled records into newline-delimited NDJSON
// frames, each at most w.frameMaxBytes, and publishes ALL the chunk's frames in a
// SINGLE async batch — so the whole chunk still drains in one ack round-trip (one
// PublishAsyncComplete barrier), while the NATS message COUNT drops from one per
// record to one per frame. Packing per record was the publish bottleneck; per
// frame keeps the barrier count at one and slashes the per-message overhead. A
// single record larger than the cap still ships alone (never dropped). The Hub
// splits each frame back into records. A per-frame publish failure routes EVERY
// record in that frame to handlePublishFailure (re-buffer/spill) — never a silent
// drop.
func (w *Writer) publishFramed(bp batchProducer, connIdx int, datas [][]byte, recs []*Record) {
	if len(datas) == 0 {
		return
	}
	// Build the frames and the records each frame carries, in order. Each frame
	// is sized EXACTLY up front so a single POOLED buffer holds it without
	// append-growth churn (a growing bytes.Buffer doubled-and-churned the heap;
	// a fresh exact-size alloc per publish was the single largest allocator under
	// load). The frame buffers are returned to framePool once EnqueueBatchAsync
	// has taken their bytes (see reclaimFrames below).
	var frames [][]byte
	var frameRecs [][]*Record
	var frameData [][][]byte // per-frame, per-record marshaled bytes (for failure retry)
	var frameHandles []*[]byte
	for start := 0; start < len(datas); {
		end := start
		// A binary frame opens with a 1-byte magic; each record is length-prefixed
		// (uvarint) instead of '\n'-terminated, since binary bodies can contain any
		// byte including '\n'. A JSON frame is newline-delimited NDJSON.
		size := 0
		if w.wireBinary {
			size = 1 // frame magic byte
		}
		for end < len(datas) {
			need := recordFrameOverhead(w.wireBinary, len(datas[end])) + len(datas[end])
			// Always include at least one record, even if it alone exceeds the cap.
			if end > start && size+need > w.frameMaxBytes {
				break
			}
			size += need
			end++
		}
		frame, handle := acquireFrame(size) // pooled, zero-length, room for size bytes
		if w.wireBinary {
			frame = append(frame, mq.BinwireMagic)
			for i := start; i < end; i++ {
				frame = binary.AppendUvarint(frame, uint64(len(datas[i])))
				frame = append(frame, datas[i]...)
			}
		} else {
			for i := start; i < end; i++ {
				frame = append(frame, datas[i]...)
				frame = append(frame, '\n')
			}
		}
		*handle = frame // keep the (possibly regrown) backing array on the handle
		frames = append(frames, frame)
		frameHandles = append(frameHandles, handle)
		frameRecs = append(frameRecs, recs[start:end])
		frameData = append(frameData, datas[start:end])
		start = end
	}
	// EnqueueBatchAsync waits for the broker ack before returning, so every frame's
	// bytes are fully taken (copied to the wire) by the time it does — whether the
	// publish succeeded or failed, the buffers are no longer referenced and return
	// to the pool. This mirrors the non-framed path's unconditional reclaimMsgBufs.
	defer reclaimFrames(frameHandles)

	ctx, cancel := context.WithTimeout(context.Background(), batchPublishTimeout)
	errs, err := w.batchPublish(bp, connIdx, ctx, frames)
	cancel()
	if err != nil {
		w.logger.Error("audit: MQ frame batch enqueue failed", "error", err, "frames", len(frames))
		for fi, fr := range frameRecs {
			for ri, rec := range fr {
				w.metrics.incEnqueueErrors()
				w.handlePublishFailure(frameData[fi][ri], rec)
			}
		}
		return
	}
	for k, fr := range frameRecs {
		if k < len(errs) && errs[k] != nil {
			for ri, rec := range fr {
				w.metrics.incEnqueueErrors()
				w.handlePublishFailure(frameData[k][ri], rec)
			}
			continue
		}
		for range fr {
			w.metrics.incEnqueueTotal()
			// Terminal: published OK. The pooled body was already reclaimed at
			// marshal time (the frame copied the marshaled bytes), so nothing to
			// reclaim here.
		}
	}
}

// publishChunk publishes one already-marshaled, byte-bounded batch and routes
// per-record failures (enqueue error, async-nak, or deadline) to
// handlePublishFailure — never a silent drop.
func (w *Writer) publishChunk(bp batchProducer, connIdx int, datas [][]byte, recs []*Record) {
	if len(datas) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), batchPublishTimeout)
	errs, err := w.batchPublish(bp, connIdx, ctx, datas)
	cancel()
	if err != nil {
		w.logger.Error("audit: MQ batch enqueue failed", "error", err, "records", len(recs))
		for j, rec := range recs {
			w.metrics.incEnqueueErrors()
			w.handlePublishFailure(datas[j], rec)
		}
		return
	}
	for j, e := range errs {
		if e != nil {
			w.metrics.incEnqueueErrors()
			w.handlePublishFailure(datas[j], recs[j])
			continue
		}
		w.metrics.incEnqueueTotal()
		// Terminal: published OK. The pooled body was already reclaimed at marshal
		// time (EnqueueBatchAsync took the bytes), so nothing to reclaim here.
	}
}

// marshalRecord builds the wire message for rec and encodes it into a POOLED
// buffer, returning the encoded bytes together with the buffer that backs them.
// The bytes ALIAS the buffer (no per-record right-sized copy — that copy-out was
// the single largest audit-path allocation), so the CALLER MUST return the
// buffer via reclaimMsgBuf once the bytes are no longer referenced:
//   - async batch path: after EnqueueBatchAsync returns (it waits for the broker
//     ack, so the payload has been fully taken; held buffers are bounded to one
//     in-flight chunk);
//   - framed path: after the frames are built (each frame copies the bytes);
//   - per-record fallback: copy the bytes out first, then reclaim immediately
//     (sync Enqueue's contract does not guarantee a synchronous copy).
//
// Returns ok=false (buffer already reclaimed) on a hard marshal failure.
func (w *Writer) marshalRecord(rec *Record) (data []byte, buf *bytes.Buffer, ok bool) {
	// A re-buffered record carries its already-encoded bytes (its pooled body was
	// reclaimed at the first marshal). Re-publish those bytes verbatim — never
	// re-read the reclaimed body. buf is nil (the bytes are not pool-backed);
	// reclaimMsgBuf(nil) is a safe no-op.
	if rec.marshaled != nil {
		return rec.marshaled, nil, true
	}

	if w.wireBinary {
		return w.marshalRecordBinary(rec)
	}

	msg := w.recordToMessage(rec)

	// Splice: detach large inline request/response bodies of any encoding (raw /
	// text / base64), arming them to emit a tiny marker so Body.MarshalJSON encodes
	// a tiny envelope instead of a fresh ~body-sized one. The real bytes are
	// rendered directly into the output buffer below (verbatim / escaped / base64).
	reqReal, reqEnc, _ := msg.RequestBody.DetachForSplice(reqBodySpliceMarker)
	respReal, respEnc, _ := msg.ResponseBody.DetachForSplice(respBodySpliceMarker)

	enc := msgBufPool.Get().(*bytes.Buffer)
	enc.Reset()
	if err := json.NewEncoder(enc).Encode(msg); err != nil {
		w.logger.Error("audit: marshal failed", "requestId", rec.RequestID, "error", err)
		reclaimMsgBuf(enc)
		w.reclaimRecordBody(rec) // dropped record: return its pooled body to the pool
		return nil, nil, false
	}
	// json.Encoder appends a trailing '\n'; drop it so each record is a single
	// line — a clean NDJSON record when the publish path packs records into frames.
	eb := enc.Bytes()
	if n := len(eb); n > 0 && eb[n-1] == '\n' {
		eb = eb[:n-1]
	}

	if reqReal == nil && respReal == nil {
		// Body bytes (if any) were encoded inline into eb; the pooled body buffer is
		// no longer referenced — reclaim it now so the pool working set tracks
		// in-flight marshals, not queue depth (the audit body pool was 92% of the
		// retained heap under load). On a publish failure the retry re-publishes
		// these bytes (rec.marshaled), never re-reading the reclaimed body.
		w.reclaimRecordBody(rec)
		return eb, enc, true // nothing detached; enc backs the result
	}

	// Splice the real bodies in place of the markers, into a second pooled buffer.
	// reqReal/respReal alias the pooled body buffers; AppendInlineForSplice copies
	// them into out, so the body is fully captured in out's bytes before reclaim.
	out := msgBufPool.Get().(*bytes.Buffer)
	out.Reset()
	if !spliceBodyMarkers(out, eb, splicedBody{reqReal, reqEnc}, splicedBody{respReal, respEnc}) {
		// A marker collided with real content (astronomically rare): re-encode
		// this record WITHOUT splicing so the bytes are always correct. The body is
		// still live here (not yet reclaimed) so marshalRecordPlain can re-read it.
		reclaimMsgBuf(out)
		reclaimMsgBuf(enc)
		return w.marshalRecordPlain(rec)
	}
	reclaimMsgBuf(enc)       // marker-encoded buffer is no longer referenced
	w.reclaimRecordBody(rec) // body now copied into out; reclaim the pooled buffer
	return out.Bytes(), out, true
}
