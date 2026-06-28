//go:build vectorscan

// Vectorscan (cgo) implementation of the Matcher seam — the production
// accelerator. All compatible patterns compile into one database scanned in a
// single pass; the RE2 matcher remains the differential oracle and the residual
// for patterns Vectorscan cannot serve. Built only under the `vectorscan` tag
// so the default toolchain (no libhs, no cgo) keeps building with the RE2 path.
//
// Engine choice: a thin hand-rolled cgo wrapper rather than an off-the-shelf
// binding. Two reasons drive it. (1) Matches are collected into a C-side buffer
// and copied across the cgo boundary once per segment scan, not once per match —
// the per-match boundary crossing is the dominant cgo cost on match-dense
// (abuse) input, and the buffer eliminates it. (2) It keeps the shared/
// dependency surface minimal and gives full control over pattern-ID encoding,
// flag mapping, the scratch pool, and (later) database serialization for the
// off-request-path prewarm swap.
//
// Scope of THIS matcher: block-mode detection. It reports which patterns fired
// in which segment (the only thing RulePackEngine.Execute consumes). It is
// compiled without start-of-match, so Hit.End is the real match end but
// Hit.Start is 0; precise redaction spans require the separate SOM database and
// the RE2 sub-group residual, not this matcher.
//
// See docs/superpowers/specs/2026-06-22-rulepack-engine-perf-design.md §3.
package matcher

/*
#include <hs.h>
#include <stdlib.h>

// nexus_hit mirrors a single Vectorscan match event. from is 0 without SOM.
typedef struct { unsigned int id; unsigned long long from; unsigned long long to; } nexus_hit;

// nexus_ctx is the per-scan collection buffer. The match callback appends into
// it entirely in C, so a scan that produces N matches crosses the cgo boundary
// once (to copy the buffer out) instead of N times.
typedef struct { nexus_hit *buf; size_t len; size_t cap; } nexus_ctx;

static int nexus_on_match(unsigned int id, unsigned long long from,
                          unsigned long long to, unsigned int flags, void *context) {
    (void)flags;
    nexus_ctx *c = (nexus_ctx *)context;
    if (c->len == c->cap) {
        size_t ncap = c->cap ? c->cap * 2 : 16;
        nexus_hit *nb = (nexus_hit *)realloc(c->buf, ncap * sizeof(nexus_hit));
        if (!nb) {
            return 1; // abort scan on allocation failure
        }
        c->buf = nb;
        c->cap = ncap;
    }
    c->buf[c->len].id = id;
    c->buf[c->len].from = from;
    c->buf[c->len].to = to;
    c->len++;
    return 0; // continue scanning
}

// nexus_scan runs one block-mode scan, collecting all matches into ctx.
static hs_error_t nexus_scan(const hs_database_t *db, const char *data,
                             unsigned int len, hs_scratch_t *scratch, nexus_ctx *ctx) {
    return hs_scan(db, data, len, 0, scratch, nexus_on_match, ctx);
}
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// cgoScanSem optionally bounds the number of concurrent Vectorscan cgo crossings
// to NEXUS_CGO_SCAN_LIMIT. A long cgo call (a 50KB scan is ~300µs, well past the
// ~20µs sysmon threshold) makes the runtime retake the M's P; on return the M
// must reacquire a P and parks if none is free. At high request concurrency
// hundreds of goroutines cross into C at once, so the M park/unpark storm
// (over-subscription) dominates the scan's wall-clock — measured ~4ms for ~140µs
// of actual scan CPU. Capping in-flight cgo scans at ≈ the core count converts
// that expensive M-handoff churn into a cheap channel wait: only N goroutines are
// ever blocked in C, so the runtime never over-subscribes. nil (the default, no
// env set) leaves behaviour unchanged.
var (
	cgoScanSem     chan struct{}
	cgoScanSemOnce sync.Once
)

// cgoScanLimit resolves NEXUS_CGO_SCAN_LIMIT: unset/"auto" → adaptiveCgoScanLimit
// (DEFAULT — A/B-validated; caps concurrent cgo scans to tame the M-oversubscription
// storm, cutting the hooks-ON p99 tail ~2.6x at flat peak RPS); "0"/neg → 0; N → N.
func cgoScanLimit() int {
	v := strings.TrimSpace(os.Getenv("NEXUS_CGO_SCAN_LIMIT"))
	if v == "" || strings.EqualFold(v, "auto") {
		return adaptiveCgoScanLimit(runtime.GOMAXPROCS(0))
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 0
}

// adaptiveCgoScanLimit caps concurrent cgo scans a little below the available
// parallelism, so the scheduler always keeps spare Ps to run Go-side goroutines
// (network I/O, audit, GC) while up to N Ms sit blocked in C. Without this, at
// high request concurrency every goroutine crosses into cgo at once and the
// returning Ms can't reacquire a P, producing the M-oversubscription park storm.
// Backs off max(2, procs/8) — a small reserve on small boxes, proportional on
// large ones — and never goes below 1.
func adaptiveCgoScanLimit(procs int) int {
	if procs < 1 {
		procs = 1
	}
	backoff := procs / 8
	if backoff < 2 {
		backoff = 2
	}
	n := procs - backoff
	if n < 1 {
		n = 1
	}
	return n
}

func cgoScanLimiter() chan struct{} {
	cgoScanSemOnce.Do(func() {
		if n := cgoScanLimit(); n > 0 {
			cgoScanSem = make(chan struct{}, n)
		}
	})
	return cgoScanSem
}

// vectorscanMatcher owns one compiled block-mode database and a pool of scratch
// spaces (Vectorscan scratch is not safe for concurrent use, so each in-flight
// Scan borrows its own). The database is immutable for the matcher's life and
// safe for concurrent Scan.
//
// Concurrency with Close: a config swap frees the evicted matcher while request
// goroutines may still be scanning it. Scan refcounts itself (lock-free on the
// hot path) and Close drains in-flight scans before freeing the database and
// scratch — so a live swap is just "install new pointer, Close old"
// with no use-after-free, no external synchronization required.
type vectorscanMatcher struct {
	db       *C.hs_database_t // read only under an inflight refcount; freed by Close
	residual Matcher          // RE2 fallback for patterns Vectorscan cannot serve; nil if none

	// idle is a GC-stable ring of reusable scratch spaces (Vectorscan scratch is
	// not safe for concurrent use, so each in-flight Scan borrows its own). A
	// buffered channel — not sync.Pool — because sync.Pool is cleared on every GC,
	// and re-creating scratch means hs_alloc_scratch, a >20µs cgo call that parks
	// the M (pthread_cond_wait) on the hot path. The channel survives GC, so under
	// steady load no scratch is ever re-allocated. Sized to cover typical per-
	// matcher request concurrency; a spike beyond cap allocates+frees transiently
	// (graceful) rather than blocking. The channel is also the sole owner: every
	// live scratch is either parked here, borrowed by an in-flight scan, or already
	// freed — so Close (after inflight drains) just drains and frees the ring, with
	// no mutex and no separate tracking slice.
	idle chan *C.hs_scratch_t

	inflight atomic.Int64 // number of Scans currently touching db/scratch
	closed   atomic.Bool  // set once by Close; gates new Scans and allocations
}

// scratchRingSize bounds the parked scratch spaces per matcher. It covers the
// expected concurrent-Scan fan-out for one compiled database; beyond it, scans
// allocate and free a transient scratch rather than block.
const scratchRingSize = 256

// flagBits maps the rule-pack flag convention (see core.CompilePattern) onto
// Vectorscan compile flags. "U" (ungreedy) has no Vectorscan equivalent and is
// reported as unmappable so the pattern falls to the RE2 residual rather than
// matching with the wrong semantics. \w/\d/\b are left ASCII (no UTF8/UCP) to
// mirror RE2's default Perl-class semantics; any residual divergence is caught
// by the differential gate and routed to the residual.
func flagBits(flags string) (C.uint, error) {
	var bits C.uint
	for _, f := range flags {
		switch f {
		case 'i':
			bits |= C.HS_FLAG_CASELESS
		case 's':
			bits |= C.HS_FLAG_DOTALL
		case 'm':
			bits |= C.HS_FLAG_MULTILINE
		case 'U':
			return 0, fmt.Errorf("vectorscan: ungreedy flag 'U' is unsupported")
		default:
			return 0, fmt.Errorf("vectorscan: unsupported regex flag %q", f)
		}
	}
	return bits, nil
}

// CompileVectorscan builds the hybrid matcher: Vectorscan serves every pattern
// it can compile, and an RE2 residual serves the rest (flag-unmappable, or
// Vectorscan-uncompilable such as backreference / lookaround). Every
// RE2-compilable pattern therefore always has a working execution path — the
// accelerator never costs detection coverage (spec §3/§4). `bad` holds only
// patterns RE2 itself cannot compile, matching the RE2 matcher's fail-posture.
//
// This guards COMPILE-level incompatibility. Patterns that compile under
// both engines but whose match SEMANTICS diverge (e.g. a bare `$` matching
// before a trailing newline under Vectorscan) are the save-time linter's
// responsibility and are pinned by the differential gate; the shipped
// rule corpus is proven divergence-free.
func CompileVectorscan(pats []Pattern) (Matcher, []BadPattern) {
	// Partition by flag-mappability; unmappable flags go straight to the residual.
	prep := make([]preparedPattern, 0, len(pats))
	var residualPats []Pattern
	for _, p := range pats {
		bits, err := flagBits(p.Flags)
		if err != nil {
			residualPats = append(residualPats, p)
			continue
		}
		prep = append(prep, preparedPattern{pat: p, flags: bits})
	}

	db, compileBad := compileDB(prep)
	// Vectorscan-uncompilable patterns also fall to the residual.
	if len(compileBad) > 0 {
		byID := make(map[int]Pattern, len(pats))
		for _, p := range pats {
			byID[p.ID] = p
		}
		for _, b := range compileBad {
			residualPats = append(residualPats, byID[b.ID])
		}
	}

	var residual Matcher
	var bad []BadPattern
	if len(residualPats) > 0 {
		residual, bad = CompileRE2(residualPats)
	}

	m := &vectorscanMatcher{db: db, residual: residual, idle: make(chan *C.hs_scratch_t, scratchRingSize)}
	return m, bad
}

// acquireScratch borrows a scratch space: a parked one if the ring has any,
// otherwise a freshly allocated one. Only called from scanDB while the inflight
// refcount is held, so m.db cannot be freed under it (Close waits for inflight to
// drain). Returns nil only if allocation fails, which scanDB treats as a no-op
// scan (fail-open: the RE2 residual, if any, still runs).
func (m *vectorscanMatcher) acquireScratch() *C.hs_scratch_t {
	select {
	case s := <-m.idle:
		return s
	default:
	}
	var s *C.hs_scratch_t
	if m.db == nil || C.hs_alloc_scratch(m.db, &s) != C.HS_SUCCESS {
		return (*C.hs_scratch_t)(nil)
	}
	return s
}

// releaseScratch returns a scratch to the ring, or frees it if the ring is full
// (a concurrency spike beyond scratchRingSize). Never blocks.
func (m *vectorscanMatcher) releaseScratch(s *C.hs_scratch_t) {
	if s == nil {
		return
	}
	select {
	case m.idle <- s:
	default:
		C.hs_free_scratch(s)
	}
}

// preparedPattern is the compiler's view of one candidate after flag mapping.
type preparedPattern struct {
	pat   Pattern
	flags C.uint
}

// compileDB compiles the prepared set into one database, peeling off any pattern
// hs_compile_multi rejects until the remainder compiles. The fast path (every
// pattern compiles) is a single hs_compile_multi call; rejection drops exactly
// the offending expression and retries. If Vectorscan reports an error it cannot
// attribute to one expression, the whole remaining set degrades to the RE2
// residual (spec §9: a build failure degrades to full-scan, never a security
// loss) rather than guessing.
func compileDB(prep []preparedPattern) (*C.hs_database_t, []BadPattern) {
	var bad []BadPattern
	for len(prep) > 0 {
		db, badIdx, err := tryCompileMulti(prep)
		if err == nil {
			return db, bad
		}
		if badIdx < 0 || badIdx >= len(prep) {
			for _, p := range prep {
				bad = append(bad, BadPattern{ID: p.pat.ID, Err: err})
			}
			return nil, bad
		}
		bad = append(bad, BadPattern{ID: prep[badIdx].pat.ID, Err: err})
		prep = append(prep[:badIdx:badIdx], prep[badIdx+1:]...)
	}
	return nil, bad
}

// tryCompileMulti attempts one batch compile. On failure it returns the
// zero-based offending expression index (or -1 if Vectorscan could not
// attribute it) and the error message.
func tryCompileMulti(prep []preparedPattern) (*C.hs_database_t, int, error) {
	n := len(prep)
	exprs := make([]*C.char, n)
	flags := make([]C.uint, n)
	ids := make([]C.uint, n)
	for i, p := range prep {
		exprs[i] = C.CString(boundForDetection(p.pat.Expr)) // detection-only repeat cap; rule.Pattern kept for redaction
		flags[i] = p.flags
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
		C.uint(n),
		C.HS_MODE_BLOCK,
		nil,
		&db,
		&cErr,
	)
	if rc == C.HS_SUCCESS {
		return db, -1, nil
	}
	idx := -1
	msg := "vectorscan: compile failed"
	if cErr != nil {
		idx = int(cErr.expression)
		if cErr.message != nil {
			msg = "vectorscan: " + C.GoString(cErr.message)
		}
		C.hs_free_compile_error(cErr)
	}
	return nil, idx, fmt.Errorf("%s", msg)
}

// Scan runs the Vectorscan database and the RE2 residual over every segment and
// merges their hits. The two cover disjoint pattern sets, so a simple
// concatenation is correct. With no start-of-match compiled in, Vectorscan hits
// carry Hit.Start=0 and Hit.End=match-end; residual hits carry RE2's real spans.
// When firstOnly is set, at most one hit per (pattern,segment) is returned —
// enough for a detect/block decision.
func (m *vectorscanMatcher) Scan(segments []string, firstOnly bool) []Hit {
	hits, _ := m.scanComplete(segments, firstOnly)
	return hits
}

// ScanComplete is Scan plus a completeness flag: complete is false when the cgo
// scan aborted mid-stream (an allocation failure in the match callback leaves a
// PARTIAL hit set). A detect/block decision tolerates a partial set (it only
// needs presence), but redaction must not — a dropped hit means a rule whose PII
// would not be masked. The rule-pack redaction path consults this to fail safe.
// Satisfies the CompleteScanner interface.
func (m *vectorscanMatcher) ScanComplete(segments []string, firstOnly bool) ([]Hit, bool) {
	return m.scanComplete(segments, firstOnly)
}

func (m *vectorscanMatcher) scanComplete(segments []string, firstOnly bool) ([]Hit, bool) {
	if len(segments) == 0 {
		return nil, true
	}
	// Enter the refcount before touching db/scratch; the deferred decrement is
	// registered first so it runs LAST (after the scratch is returned to the
	// pool), guaranteeing Close cannot observe inflight==0 until this scan has
	// fully released its scratch. m.db is read only inside this gate, so a
	// concurrent Close (which frees m.db only after inflight drains) never races
	// the read.
	m.inflight.Add(1)
	defer m.inflight.Add(-1)
	if m.closed.Load() {
		return nil, true
	}

	var hits []Hit
	complete := true
	if m.db != nil {
		var dbHits []Hit
		dbHits, complete = m.scanDB(segments, firstOnly)
		hits = dbHits
	}
	if m.residual != nil {
		// The RE2 residual never truncates, so it does not affect completeness.
		hits = append(hits, m.residual.Scan(segments, firstOnly)...)
	}
	return hits, complete
}

// scanDB runs the Vectorscan database over the segments. Caller holds the
// inflight refcount and has confirmed m.db != nil.
func (m *vectorscanMatcher) scanDB(segments []string, firstOnly bool) (hits []Hit, complete bool) {
	// Bound concurrent cgo crossings (if configured) BEFORE borrowing scratch, so a
	// goroutine queued for a slot does not pin a scratch while it waits.
	if sem := cgoScanLimiter(); sem != nil {
		sem <- struct{}{}
		defer func() { <-sem }()
	}
	scratch := m.acquireScratch()
	if scratch == nil {
		// Scratch allocation failed → the scan never ran. Report incomplete so the
		// redaction path fails safe (same class as a mid-scan abort: matches may be
		// missing); a detect/block caller still gets an empty, presence-only result.
		return nil, false
	}
	defer m.releaseScratch(scratch)

	complete = true
	for si, seg := range segments {
		var ctx C.nexus_ctx
		var data *C.char
		if len(seg) > 0 {
			data = (*C.char)(unsafe.Pointer(unsafe.StringData(seg)))
		} else {
			data = (*C.char)(unsafe.Pointer(&emptyByte))
		}
		rc := C.nexus_scan(m.db, data, C.uint(len(seg)), scratch, &ctx)
		if ctx.buf != nil {
			n := int(ctx.len)
			raw := unsafe.Slice((*C.nexus_hit)(unsafe.Pointer(ctx.buf)), n)
			var seen map[int]struct{}
			if firstOnly {
				seen = make(map[int]struct{}, n)
			}
			for i := 0; i < n; i++ {
				id := int(raw[i].id)
				if firstOnly {
					if _, dup := seen[id]; dup {
						continue
					}
					seen[id] = struct{}{}
				}
				hits = append(hits, Hit{ID: id, Seg: si, Start: int(raw[i].from), End: int(raw[i].to)})
			}
			C.free(unsafe.Pointer(ctx.buf))
		}
		// HS_SCAN_TERMINATED means the match callback aborted on an allocation
		// failure, leaving a PARTIAL hit set for this segment. Benign for a
		// presence-only detect/block decision, but the redaction path treats
		// !complete as fail-unsafe and re-localises every rule.
		if rc != C.HS_SUCCESS {
			complete = false
		}
	}
	return hits, complete
}

// emptyByte is a valid, never-dereferenced address handed to hs_scan for a
// zero-length segment (hs_scan reads no bytes when length is 0).
var emptyByte byte

// Close frees the database and every scratch. It is safe to call while other
// goroutines are scanning: Close marks the matcher closed (new Scans bail), then
// waits for in-flight scans to drain before freeing, so a live config swap
// never frees memory a scan is still using. Idempotent. Returns nil to
// satisfy io.Closer (the eviction path type-asserts hooks to io.Closer).
func (m *vectorscanMatcher) Close() error {
	if m.closed.Swap(true) {
		return nil // already closed (or closing)
	}
	// Wait for in-flight scans to release db + scratch. Close runs off the hot
	// path (config swap), so a brief yield-spin is fine; no Scan can newly enter
	// once closed is set, so this terminates.
	for m.inflight.Load() > 0 {
		runtime.Gosched()
	}
	// inflight == 0 now, so no scan holds a scratch: every live scratch is parked
	// in the ring (overflow scratches were already freed by releaseScratch). Drain
	// and free them, then the database.
	for {
		select {
		case s := <-m.idle:
			C.hs_free_scratch(s)
			continue
		default:
		}
		break
	}
	if m.db != nil {
		C.hs_free_database(m.db)
		m.db = nil
	}
	return nil
}
