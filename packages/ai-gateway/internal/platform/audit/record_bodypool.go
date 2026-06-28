package audit

import "sync"

// requestBodyPool reuses the per-request captured-body buffer. readBody must take
// a right-sized copy of the request body out of its (reused) read scratch because
// the body escapes to the async audit writer; that copy was the single largest
// hot-path allocation (~19.5 GB/window on 50 KB prompts). Pooling it lets the
// audit writer return the buffer once the record is terminally resolved
// (published / dead-lettered / dropped), so it is reused instead of GC'd.
//
// Ownership: AcquireRequestBody hands back a pooled buffer plus its handle. When
// the body is CAPTURED for audit, the handle is attached to the Record and the
// writer reclaims it the MOMENT the record is marshaled (the marshaled bytes hold
// a full copy), NOT at publish-OK — so the pool working set is O(in-flight
// marshals), not O(queue depth). A publish failure re-buffers the marshaled bytes
// (rec.marshaled), never re-reading the reclaimed body. When the body is NOT
// captured, the caller drops the handle and the buffer is GC'd (correct, just
// unpooled for that path).
var requestBodyPool = sync.Pool{New: func() any { b := make([]byte, 0, 64<<10); return &b }}

// requestBodyPoolCap bounds the capacity a body buffer may have and still return
// to the pool — one oversized request must not inflate every pooled buffer.
const requestBodyPoolCap = 2 << 20 // 2 MiB

// AcquireRequestBody returns a pooled, right-sized copy of src for use as
// Record.RequestBody, plus the handle the writer reclaims at the audit terminal.
// The handle is nil when src is empty (nothing to pool).
func AcquireRequestBody(src []byte) (body []byte, handle *[]byte) {
	if len(src) == 0 {
		return nil, nil
	}
	hp := requestBodyPool.Get().(*[]byte)
	b := *hp
	if cap(b) < len(src) {
		b = make([]byte, len(src))
	} else {
		b = b[:len(src)]
	}
	copy(b, src)
	*hp = b
	return b, hp
}

// AttachPooledRequestBody records that rec.RequestBody points at a pooled buffer,
// so the writer reclaims it at the record's terminal resolution. Call this only
// when the body bytes ARE the pooled buffer from AcquireRequestBody and the record
// will be enqueued for audit; otherwise drop the handle (the buffer GC's).
func (r *Record) AttachPooledRequestBody(handle *[]byte) {
	r.reqBodyHandle = handle
}

// releaseRequestBody returns a body buffer to the pool unless it ballooned past
// the cap (dropped to GC instead).
func releaseRequestBody(hp *[]byte) {
	if hp == nil {
		return
	}
	if cap(*hp) <= requestBodyPoolCap {
		requestBodyPool.Put(hp)
	}
}

// reclaimRecordBody returns a record's pooled request/response body buffers to
// their pools. Called the moment the record is marshaled (the marshaled bytes hold
// a full copy), so the buffers cycle back immediately instead of being pinned for
// the whole publish window. Idempotent: a no-op for records whose body was not
// pooled, or already reclaimed (the handles are nil'd here). After this runs the
// record's body bytes MUST NOT be read again — a re-buffered retry re-publishes
// the marshaled bytes (rec.marshaled) instead.
func (w *Writer) reclaimRecordBody(rec *Record) {
	if rec == nil {
		return
	}
	if rec.reqBodyHandle != nil {
		releaseRequestBody(rec.reqBodyHandle)
		rec.reqBodyHandle = nil
		rec.RequestBody = nil // defensive: never read the reclaimed bytes
	}
	if rec.respBodyHandle != nil {
		releaseResponseBody(rec.respBodyHandle)
		rec.respBodyHandle = nil
		rec.ResponseBody = nil // defensive: never read the reclaimed bytes
	}
}

// responseBodyPool reuses the streaming-capture tee's backing array. The SSE
// capture tee buffers the response body into this array (one alloc per stream
// was ~3.7 GB/window — the second-largest streaming-relay allocator); pooling it
// lets the audit writer return the buffer once the record is terminally resolved
// (published / spilled / dropped) so it is reused instead of GC'd. The tee
// appends THROUGH the handle (*[]byte), so a body that outgrows the initial
// capacity keeps the handle pointing at the regrown array.
var responseBodyPool = sync.Pool{New: func() any { b := make([]byte, 0, 16<<10); return &b }}

// responseBodyPoolCap bounds the capacity a tee buffer may have and still return
// to the pool — one oversized response must not inflate every pooled buffer.
const responseBodyPoolCap = 2 << 20 // 2 MiB

// AcquireResponseBuffer returns a pooled, zero-length buffer handle for the
// streaming capture tee to append into. The handle is attached to the Record via
// AttachPooledResponseBody when the body is captured for audit, or returned via
// ReleaseResponseBuffer when the stream's body is not stored.
func AcquireResponseBuffer() *[]byte {
	hp := responseBodyPool.Get().(*[]byte)
	*hp = (*hp)[:0]
	return hp
}

// ReleaseResponseBuffer returns a tee buffer to the pool directly (used when the
// captured body is NOT attached to an audit record, so no terminal reclaim runs).
// Buffers grown past the cap are dropped to GC instead of pooled.
func ReleaseResponseBuffer(hp *[]byte) { releaseResponseBody(hp) }

// AttachPooledResponseBody records that rec.ResponseBody points at the pooled tee
// buffer behind handle, so the writer reclaims it at the record's terminal
// resolution. Call this only when rec.ResponseBody IS *handle and the record will
// be enqueued for audit; otherwise ReleaseResponseBuffer the handle instead.
func (r *Record) AttachPooledResponseBody(handle *[]byte) {
	r.respBodyHandle = handle
}

// releaseResponseBody returns a tee buffer to the pool unless it ballooned past
// the cap (dropped to GC instead).
func releaseResponseBody(hp *[]byte) {
	if hp == nil {
		return
	}
	if cap(*hp) <= responseBodyPoolCap {
		responseBodyPool.Put(hp)
	}
}
