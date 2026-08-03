// msg_buf_pool.go — the reusable marshal buffer flushBatch publishes each audit
// event from. Separate from mq_writer.go because its correctness argument is
// self-contained: who owns the buffer, when it goes back, and which buffers are
// too big to keep.
package audit

import (
	"bytes"
	"sync"
)

// msgBufPool reuses the per-event marshal buffer flushBatch publishes from.
//
// OWNERSHIP: flushBatch acquires a buffer, encodes one TrafficEventMessage into
// it, hands the ALIASED bytes to producer.Enqueue, and returns the buffer via
// reclaimMsgBuf as soon as Enqueue returns — on the error path too. No other
// goroutine ever sees the buffer, and nothing downstream retains the bytes:
// Enqueue's contract requires the payload to be taken before it returns, which
// the sole production implementation satisfies by construction (NATSProducer
// publishes through jetstream.Publish, a synchronous request/reply whose payload
// nats.go copies into the connection's write buffer before the call completes).
// The NDJSON fallback never sees these bytes — it re-marshals from the AuditEvent
// slice, which is why reclaiming before the error return is safe.
//
// sync.Pool rather than a buffer field on the writer: flushBatch normally runs
// only on the loop goroutine, but Flush calls it directly once the loop has
// exited, and Flush has no single-caller guarantee — a shared field would be a
// data race there. The buffer is a plain bytes.Buffer, cheap to recreate, so
// playbook 2.2 puts it on the sync.Pool side of the decision rule rather than the
// GC-stable-ring side.
//
// Deliberately NOT pre-grown, unlike the ai-gateway's equivalent. Encode issues a
// single Write of the finished message, so an empty bytes.Buffer sizes itself
// exactly once and a pre-grow only changes what a pool MISS costs — measured
// strictly worse at every size: an envelope-only event pays 68,310 B against
// 3,707 B for the same 9 allocations with a 64 KiB pre-grow, and an 8 KiB-body
// event 87,939 B against 40,402 B. Steady-state reuse is identical either way
// (Reset keeps the capacity), so the pre-grow buys nothing and costs the whole
// class size every time the GC empties the pool.
var msgBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// msgBufReclaimCap bounds the capacity a buffer may have and still return to the
// pool. One oversized captured body must not inflate every pooled buffer for the
// life of the process.
const msgBufReclaimCap = 4 << 20

// msgBufPoolable reports whether b may go back into msgBufPool. A buffer that
// ballooned past msgBufReclaimCap servicing one oversized captured body is dropped
// to GC instead — pooling it would make every later event, however small, hold that
// capacity for the life of the process.
func msgBufPoolable(b *bytes.Buffer) bool { return b != nil && b.Cap() <= msgBufReclaimCap }

// reclaimMsgBuf returns a marshal buffer to the pool once its bytes are no longer
// referenced. Nil-safe.
func reclaimMsgBuf(b *bytes.Buffer) {
	if msgBufPoolable(b) {
		msgBufPool.Put(b)
	}
}
