//go:build vectorscan

// Streaming-mode prefilter. See StreamScanner in matcher.go for why this exists
// and what it deliberately does not do.
//
// A second compiled database, in HS_MODE_STREAM, over the same patterns as the
// block-mode one. Two databases rather than one because the modes answer
// different questions and the block one is not going away: the streaming scan
// says "something may have matched by now", cheaply, per arriving chunk; the
// block scan says WHERE it matched, expensively, over accumulated text, and only
// once the streaming scan has said to look. Start-of-match — the thing a
// redaction needs and the thing streaming mode charges most for — is therefore
// never asked of this database.
package matcher

/*
#cgo pkg-config: libhs
#include <hs.h>
#include <stdlib.h>

// nexus_stream_seen sets a flag and stops the scan. The prefilter answers a
// boolean, so the first match ends the work for this write.
static int nexus_stream_seen(unsigned int id, unsigned long long from,
                             unsigned long long to, unsigned int flags, void *ctx) {
    *(int *)ctx = 1;
    return 1;
}

static int nexus_scan_stream(hs_stream_t *s, hs_scratch_t *sc,
                             const char *data, unsigned int len, int *seen) {
    return hs_scan_stream(s, data, len, 0, sc, nexus_stream_seen, seen);
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// compileStreamDB builds the HS_MODE_STREAM database over the patterns that
// already compiled in block mode.
//
// Patterns are not re-partitioned. Anything the block compile rejected is
// already on the RE2 residual, and the residual has no streaming path — so a
// caller with a residual must keep accumulating for it regardless. Compiling
// the same set keeps the two databases answering about the same rules, which is
// what makes the differential gate meaningful.
//
// Returns nil (no error) when the set cannot be compiled in streaming mode:
// callers then find no StreamScanner and keep the accumulate-and-rescan path.
// A missing optimisation is not a failure.
func compileStreamDB(prep []preparedPattern) *C.hs_database_t {
	if len(prep) == 0 {
		return nil
	}
	exprs := make([]*C.char, len(prep))
	flags := make([]C.uint, len(prep))
	ids := make([]C.uint, len(prep))
	for i, p := range prep {
		exprs[i] = C.CString(p.pat.Expr)
		// SINGLEMATCH: one report per pattern is all a presence answer needs,
		// and it lets the engine drop state it would otherwise keep to report
		// later matches of the same rule.
		flags[i] = p.flags | C.HS_FLAG_SINGLEMATCH
		ids[i] = C.uint(p.pat.ID)
	}
	defer func() {
		for _, e := range exprs {
			C.free(unsafe.Pointer(e))
		}
	}()

	var db *C.hs_database_t
	var cErr *C.hs_compile_error_t
	rc := C.hs_compile_multi(
		(**C.char)(unsafe.Pointer(&exprs[0])),
		(*C.uint)(unsafe.Pointer(&flags[0])),
		(*C.uint)(unsafe.Pointer(&ids[0])),
		C.uint(len(prep)),
		C.HS_MODE_STREAM,
		nil, &db, &cErr,
	)
	if rc != C.HS_SUCCESS {
		if cErr != nil {
			C.hs_free_compile_error(cErr)
		}
		return nil
	}
	return db
}

// StreamStateBytes is the per-open-stream engine state, for a caller that has to
// bound how many concurrent streams it will hold. Zero when streaming is
// unavailable.
//
// It takes a reference for the duration: reading m.streamDB unguarded races the
// release that frees it and nils it, and hs_stream_size on a freed database is a
// segfault, not a wrong number.
func (m *vectorscanMatcher) StreamStateBytes() int {
	db, ok := m.acquireStreamDB()
	if !ok {
		return 0
	}
	defer m.releaseStreamDB()
	var n C.size_t
	if C.hs_stream_size(db, &n) != C.HS_SUCCESS {
		return 0
	}
	return int(n)
}

// acquireStreamDB takes a reference on the streaming database and returns the
// handle to use under it.
//
// The handle is returned rather than read from the struct afterwards because the
// field is what the release nils: a caller that re-reads m.streamDB after taking
// the reference can still observe nil, while the pointer it was handed stays
// valid for as long as it holds the count.
//
// The increment is a compare-and-swap that REFUSES to move the count off zero,
// which is what makes the count unresurrectable and the free single.
//
// A plain Add(1) with a closed-recheck around it does not: Close can drop the
// last reference to zero and be descheduled before the free, an acquirer can
// then lift that zero back to one, see closed, release, drive it to zero a
// SECOND time, and free — and Close, resuming, frees the same handle again.
// Both goroutines observed a legitimate 1→0 transition, so no amount of
// rechecking closed after the fact can separate them. Measured, not reasoned:
// the lifecycle gate reproduces it as a SIGABRT inside hs_free_database.
func (m *vectorscanMatcher) acquireStreamDB() (*C.hs_database_t, bool) {
	for {
		n := m.streamRefs.Load()
		// Zero means the last reference is gone or going: the database is being
		// freed and must never be handed out again. Closed is a cheap early
		// refusal on top; the zero check is the one carrying correctness.
		if n <= 0 || m.closed.Load() {
			return nil, false
		}
		if m.streamRefs.CompareAndSwap(n, n+1) {
			break
		}
	}
	db := m.loadStreamDB()
	if db == nil {
		// Cannot happen while a reference is held — the free nils the field only
		// after the count reaches zero, and the CAS above refuses to lift it off
		// zero. Released rather than asserted so a future change to that
		// invariant leaks nothing.
		m.releaseStreamDB()
		return nil, false
	}
	return db, true
}

// loadStreamDB reads the streaming database handle under streamMu. Every read of
// the field goes through here: the last release nils it, so an unguarded read is
// a race with a pointer being freed.
func (m *vectorscanMatcher) loadStreamDB() *C.hs_database_t {
	m.streamMu.Lock()
	defer m.streamMu.Unlock()
	return m.streamDB
}

// OpenScanStream implements StreamScanner.
func (m *vectorscanMatcher) OpenScanStream() (ScanStream, error) {
	if m.closed.Load() || m.loadStreamDB() == nil {
		return nil, errors.New("matcher: streaming prefilter unavailable")
	}
	// Take a reference on the streaming database for the stream's WHOLE life,
	// via streamRefs and NOT inflight. Close spin-waits on inflight, so putting
	// a seconds-long stream there would make a config swap busy-wait until the
	// response finished; streamRefs instead lets Close return and hands the
	// database's lifetime to whoever is still using it.
	//
	// The reference is taken BEFORE anything touches a database, because the
	// allocation below is exactly where a config swap used to segfault: Close
	// frees m.db and nils it while hs_alloc_scratch is reading it.
	db, ok := m.acquireStreamDB()
	if !ok {
		return nil, errors.New("matcher: closed")
	}

	var h *C.hs_stream_t
	if C.hs_open_stream(db, 0, &h) != C.HS_SUCCESS {
		m.releaseStreamDB()
		return nil, errors.New("matcher: hs_open_stream failed")
	}
	// One scratch per stream rather than one per write: scratch is not
	// concurrency-safe and hs_close_stream needs one too, so borrowing per write
	// would risk closing with a scratch the ring had handed to someone else.
	//
	// Allocated against the STREAMING database and owned by this stream. It used
	// to come from acquireScratch, which allocates against the BLOCK database and
	// parks into a ring Close drains — two defects in one line. The scan then
	// worked only because the block scratch happened to be the larger of the two;
	// a rule set where that inverts would fail every write, and the failure path
	// sets seen = true, so the prefilter would silently answer "always look
	// closer" and the optimisation would evaporate with every test still green.
	var sc *C.hs_scratch_t
	if C.hs_alloc_scratch(db, &sc) != C.HS_SUCCESS {
		C.hs_close_stream(h, nil, nil, nil)
		m.releaseStreamDB()
		return nil, errors.New("matcher: scratch allocation failed")
	}
	return &vectorscanStream{m: m, h: h, sc: sc}, nil
}

type vectorscanStream struct {
	m    *vectorscanMatcher
	h    *C.hs_stream_t
	sc   *C.hs_scratch_t
	seen bool
	done bool
}

// Write feeds one piece and reports whether anything may have matched so far.
//
// Once seen is true it stays true and no further scanning happens: the caller's
// contract is that the first true is the signal to run the authoritative scan,
// so continuing to feed the engine would burn CPU to re-learn a decision that
// has already been made.
func (s *vectorscanStream) Write(chunk []byte) (bool, error) {
	if s.done {
		return s.seen, errors.New("matcher: write to a closed scan stream")
	}
	if s.seen || len(chunk) == 0 {
		return s.seen, nil
	}
	hit := C.int(0)
	rc := C.nexus_scan_stream(s.h, s.sc,
		(*C.char)(unsafe.Pointer(&chunk[0])), C.uint(len(chunk)), &hit)
	// HS_SCAN_TERMINATED is the callback stopping at the first match, not a
	// failure.
	if rc != C.HS_SUCCESS && rc != C.HS_SCAN_TERMINATED {
		// Fail toward "look closer": a prefilter that errors must not be the
		// reason a rule goes unchecked.
		s.seen = true
		return true, errors.New("matcher: hs_scan_stream failed")
	}
	if hit != 0 {
		s.seen = true
	}
	return s.seen, nil
}

func (s *vectorscanStream) Close() error {
	if s.done {
		return nil
	}
	s.done = true
	C.hs_close_stream(s.h, s.sc, nil, nil)
	// Freed, not parked. The idle ring holds block-mode scratches and Close
	// drains it once; a scratch returned to it after that drain is one nobody
	// frees — a leak per config swap that raced a live stream.
	C.hs_free_scratch(s.sc)
	// Dropping the last reference here frees the database, which is the point:
	// a matcher closed mid-stream does not wait, the final stream cleans up.
	s.m.releaseStreamDB()
	return nil
}
