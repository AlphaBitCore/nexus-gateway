package bodyread

import (
	"bytes"
	"errors"
	"io"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Bounded sizes its buffer from a declared length (findings M-1 and C-11). These pin the
// cases where sizing differs from io.ReadAll, because most of them fail SILENTLY: a
// mis-sized buffer yields a body that is the wrong length rather than an error, and callers
// feed that body to usage extraction, to compliance hooks, or relay it verbatim to a client.
//
// They live here rather than beside either caller because the algorithm lives here — the
// whole point of the shared package is that these properties are asserted once.

func TestBounded_ExactDeclaredLength(t *testing.T) {
	const body = `{"text":"hello"}`
	got, err := Bounded(strings.NewReader(body), int64(len(body)), 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

// TestBounded_DeclaredLengthOverstated is the load-bearing case. A sender that declares
// more than it sends — or a connection cut mid-body — must not produce a body padded with
// NULs to the declared length. That padding is silent corruption: JSON stops parsing and a
// verbatim-relayed error body gains garbage, with no error surfaced.
func TestBounded_DeclaredLengthOverstated(t *testing.T) {
	const body = `{"text":"short"}`
	got, err := Bounded(strings.NewReader(body), 4096, 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil — a short body is the sender's business and the partial "+
			"body is still useful to callers", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q (len %d), want %q (len %d). A buffer sized from an overstated "+
			"declared length must be truncated to what actually arrived, or the tail is NUL "+
			"padding that silently corrupts the body.", got, len(got), body, len(body))
	}
	if bytes.IndexByte(got, 0) >= 0 {
		t.Fatalf("body contains NUL padding: %q", got)
	}
}

// TestBounded_UnknownDeclaredLength covers chunked bodies, where sizing is impossible and
// the behaviour must be exactly the io.ReadAll path this replaces.
func TestBounded_UnknownDeclaredLength(t *testing.T) {
	const body = `{"text":"chunked"}`
	got, err := Bounded(strings.NewReader(body), -1, 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

// TestBounded_OverCapDeclaredLengthIsNotTrusted is the DoS guard. A declared length above
// the cap must NOT be used as an allocation size — otherwise a header alone reserves the
// whole cap (32 MiB on the AI Gateway's STT path) without a byte being sent.
func TestBounded_OverCapDeclaredLengthIsNotTrusted(t *testing.T) {
	const body = `{"text":"tiny"}`
	const declared = 1 << 30 // 1 GiB, far above the cap
	got, err := Bounded(strings.NewReader(body), declared, 64)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
	// The property that matters is that the DECLARED length was not used as the allocation
	// size. It is not "capacity <= cap": the fallback path starts at 512 bytes regardless of
	// the cap, so a tighter bound would fail for a reason unrelated to the guard. Anything in
	// the kilobytes proves the 1 GiB header was ignored.
	if c := cap(got); c >= declared {
		t.Fatalf("allocated capacity %d matches the declared %d — an over-cap declared length was "+
			"trusted as an allocation size, so a header alone could reserve the whole cap",
			c, declared)
	}
	if c := cap(got); c > 4096 {
		t.Fatalf("allocated capacity %d for a %d-byte body: far more than the fallback path's "+
			"geometric growth should produce, which suggests the declared length leaked into "+
			"the sizing", c, len(body))
	}
}

// TestBounded_BodyExactlyFillsFirstChunk covers the boundary the chunk chain has and a single
// growing buffer did not: a body whose length is exactly the first chunk's size fills it, so the
// reader is asked for more, and that read comes back empty at EOF. The empty chunk must be
// dropped rather than chained — otherwise the result is joined into a second, redundant
// allocation and copied for nothing, on a body that already sat contiguous in one buffer.
func TestBounded_BodyExactlyFillsFirstChunk(t *testing.T) {
	// A declared length sizes the first chunk large, and the cap leaves only a sliver of room
	// after it, so the follow-on chunk EOF lands in is tiny. That makes the redundant join —
	// a second allocation the size of the whole body — the only large term in the measurement,
	// which keeps the assertion clear of the allocation-count noise -race introduces.
	const declared = 64 << 10
	const bodyLen = declared + 1 // exactly the first chunk, spare byte included
	body := strings.Repeat("e", bodyLen)

	got, err := Bounded(strings.NewReader(body), declared, bodyLen+64)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("body len = %d, want %d", len(got), bodyLen)
	}

	bytesPerRead, _ := allocCost(func() []byte {
		out, _ := Bounded(strings.NewReader(body), declared, bodyLen+64)
		return out
	})
	if bytesPerRead > bodyLen+bodyLen/2 {
		t.Fatalf("reading a %d-byte body that exactly filled the first chunk allocated %d bytes, "+
			"want under %d. The empty chunk that EOF landed in was chained anyway, so the result "+
			"was joined — a second allocation the size of the whole body, plus a copy of it, for "+
			"a body that already sat contiguous in one buffer.",
			bodyLen, bytesPerRead, bodyLen+bodyLen/2)
	}
}

// TestBounded_CapTruncates pins that the byte ceiling still bounds a body longer than the
// cap, on the unknown-length path where the cap is the only bound.
func TestBounded_CapTruncates(t *testing.T) {
	body := strings.Repeat("x", 500)
	got, err := Bounded(strings.NewReader(body), -1, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100 — the cap must still truncate", len(got))
	}
}

// TestBounded_CapTruncatesWithHonestDeclaredLength is the sized-path half of the cap guard.
// The unknown-length case above never exercises the declared-length branch, so a change that
// let `declared` widen the read window past maxBytes would slip through it.
func TestBounded_CapTruncatesWithHonestDeclaredLength(t *testing.T) {
	body := strings.Repeat("x", 500)
	got, err := Bounded(strings.NewReader(body), int64(len(body)), 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100 — the cap bounds the read even when the declared length "+
			"is honest and larger; the cap is a ceiling, not a suggestion", len(got))
	}
}

func TestBounded_NilAndEmpty(t *testing.T) {
	if got, err := Bounded(nil, 5, 1<<20); got != nil || err != nil {
		t.Fatalf("nil src = (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := Bounded(strings.NewReader("abc"), 3, 0); got != nil || err != nil {
		t.Fatalf("zero cap = (%v, %v), want (nil, nil) — a non-positive cap reads nothing", got, err)
	}
	if got, err := Bounded(strings.NewReader("abc"), 3, -1); got != nil || err != nil {
		t.Fatalf("negative cap = (%v, %v), want (nil, nil)", got, err)
	}
	got, err := Bounded(strings.NewReader(""), 0, 1<<20)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty body = (%q, %v), want (empty, nil)", got, err)
	}
}

// errAfterReader delivers some bytes and then a non-EOF error, which is what a reset
// connection mid-body looks like. Bounded must return the partial body ALONGSIDE the error
// rather than discarding it — the callers on the bumped path decide whether a partial body
// is still worth auditing, and they cannot decide that if it is thrown away here.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestBounded_NonEOFErrorReturnsPartialBody(t *testing.T) {
	want := "partial-payload"
	sentinel := io.ErrUnexpectedEOF
	got, err := Bounded(&errAfterReader{data: []byte(want), err: sentinel}, 4096, 1<<20)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v — a mid-body failure must reach the caller", err, sentinel)
	}
	if string(got) != want {
		t.Fatalf("partial body = %q, want %q. The bytes that did arrive must be returned with "+
			"the error; callers decide whether a partial body is auditable, and cannot if it is "+
			"discarded here.", got, want)
	}
}

// TestBounded_HugeDeclaredLengthDoesNotCommitMemory is the OOM guard, and it exists because
// the owner has been burned by exactly this: sizing from Content-Length without bounding it.
// A declared length is a CLAIM. Nothing stops a sender from declaring the full cap — 32 MiB
// on the STT path — and sending a few bytes, and N concurrent such bodies would commit
// N x 32 MiB before any payload arrived. io.ReadAll never had that exposure, because its
// growth is driven by bytes that actually showed up.
func TestBounded_HugeDeclaredLengthDoesNotCommitMemory(t *testing.T) {
	const declared = 32 << 20 // the STT cap: the largest a header may claim and still be "in range"
	body := strings.Repeat("x", 1024)

	got, err := Bounded(strings.NewReader(body), declared, declared)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("body length = %d, want %d", len(got), len(body))
	}
	if c := cap(got); c > PreallocCap {
		t.Fatalf("allocated capacity %d for a %d-byte body that DECLARED %d.\n"+
			"A declared length alone must not commit more than PreallocCap (%d): it is a claim, "+
			"not delivered bytes. N concurrent lying senders would otherwise commit N x the cap "+
			"before a single payload byte arrived — an exposure io.ReadAll did not have, since "+
			"its growth follows bytes that actually showed up.",
			c, len(got), declared, PreallocCap)
	}
}

// TestBounded_LargeBodyStillGrows is the other half: bounding the FIRST allocation must not
// break genuinely large bodies. Growth past PreallocCap is earned — it happens only after
// that many bytes were really delivered.
func TestBounded_LargeBodyStillGrows(t *testing.T) {
	body := strings.Repeat("y", (PreallocCap*3)+17) // deliberately not a power-of-two multiple
	got, err := Bounded(strings.NewReader(body), int64(len(body)), 32<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != len(body) {
		t.Fatalf("len = %d, want %d — bounding the first allocation must not truncate a real body",
			len(got), len(body))
	}
	if string(got) != body {
		t.Fatal("body content differs after growth")
	}
}

// TestBounded_LargeUnknownLengthBodyStillGrows is the same guard on the doubling branch.
// TestBounded_LargeBodyStillGrows always has a declared length to aim at, so it never
// exercises the `next = cap * 2` arm past the first growth.
func TestBounded_LargeUnknownLengthBodyStillGrows(t *testing.T) {
	body := strings.Repeat("w", (PreallocCap*2)+9)
	got, err := Bounded(strings.NewReader(body), -1, 32<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("len = %d, want %d — geometric growth must still reach a large chunked body",
			len(got), len(body))
	}
}

// TestBounded_HonestSmallLengthIsOneAllocation pins the win itself, so a future change that
// quietly stops sizing from the declared length fails here rather than just getting slower.
// The bodies these callers actually see — a chat completion, an STT transcript, a video job
// descriptor, a provider error body — are all in this class.
func TestBounded_HonestSmallLengthIsOneAllocation(t *testing.T) {
	body := strings.Repeat("z", 8<<10)
	got, err := Bounded(strings.NewReader(body), int64(len(body)), 32<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cap(got) > len(body)+1 {
		t.Fatalf("capacity = %d for an honestly-declared %d-byte body, want at most %d. "+
			"An honest in-range declared length should produce one exact allocation; a larger "+
			"capacity means the sizing was skipped and geometric growth ran instead. The +1 is "+
			"deliberate: it lets EOF land before the grow check, keeping an exact fit to one "+
			"allocation.",
			cap(got), len(body), len(body)+1)
	}
}

// sweepSizes spans the body sizes these call sites see, from a provider error body up to the
// largest non-streamed JSON response. The benchmarks below run all three arms over the same
// sizes so the comparison cannot drift onto a different reader or a different body.
var sweepSizes = []int{8192, 20000, 32768, 65536, 131072}

const sweepCap = 32 << 20 // the AI Gateway's STT ceiling, the widest cap in production

// BenchmarkSweep_ReadAll is the baseline both arms are measured against: the exact call this
// package replaced, kept in-package so it is benchmarked on the same reader.
func BenchmarkSweep_ReadAll(b *testing.B) {
	for _, n := range sweepSizes {
		body := strings.Repeat("x", n)
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				out, err := io.ReadAll(io.LimitReader(strings.NewReader(body), sweepCap+1))
				if err != nil || len(out) != n {
					b.Fatalf("read = %d bytes, err %v", len(out), err)
				}
				benchSink = out
			}
		})
	}
}

// BenchmarkSweep_BoundedCL is the arm this package exists for: a declared length turns the read
// into one exactly-sized allocation with no copy.
func BenchmarkSweep_BoundedCL(b *testing.B) {
	for _, n := range sweepSizes {
		body := strings.Repeat("x", n)
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				out, err := Bounded(strings.NewReader(body), int64(n), sweepCap)
				if err != nil || len(out) != n {
					b.Fatalf("read = %d bytes, err %v", len(out), err)
				}
				benchSink = out
			}
		})
	}
}

// BenchmarkSweep_BoundedNoCL is the arm production actually runs — the transport gunzips and
// reports no length — and the one TestBounded_UnknownLengthCostsNoMoreThanReadAll asserts on.
func BenchmarkSweep_BoundedNoCL(b *testing.B) {
	for _, n := range sweepSizes {
		body := strings.Repeat("x", n)
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				out, err := Bounded(strings.NewReader(body), -1, sweepCap)
				if err != nil || len(out) != n {
					b.Fatalf("read = %d bytes, err %v", len(out), err)
				}
				benchSink = out
			}
		})
	}
}

// TestBounded_UnknownLengthCostsNoMoreThanReadAll is the regression guard for the reason this
// package exists: it must not be more expensive than the io.ReadAll call it replaced.
//
// It guards the arm that is easiest to get wrong, because it is invisible in the arm everyone
// looks at. The AI Gateway denylists Accept-Encoding on the request side so the Go transport owns
// gzip, and net/http sets Response.ContentLength to -1 on every response it transparently
// gunzips — so the DECLARED-length path is the exception in production and this one is the rule.
// A growth policy tuned on the declared path can regress this one badly and still look like a win:
// a doubling policy measured here allocated +84 % at 8 KiB, +89 % at 64 KiB and +73 % at 128 KiB
// against io.ReadAll, while halving the allocation COUNT, which is the shape of trade that reads
// like an improvement in a benchmark table and is not one.
//
// A benchmark cannot fail a build, so the assertion lives in a test. The 5 % tolerance absorbs the
// few-byte jitter of runtime accounting; the regression it exists to catch is two orders of
// magnitude larger than that.
func TestBounded_UnknownLengthCostsNoMoreThanReadAll(t *testing.T) {
	for _, size := range []int{1024, 8192, 20000, 65536, 131072} {
		body := strings.Repeat("x", size)

		readAllBytes, readAllAllocs := allocCost(func() []byte {
			out, err := io.ReadAll(io.LimitReader(strings.NewReader(body), (32<<20)+1))
			if err != nil || len(out) != size {
				t.Fatalf("baseline read = %d bytes, err %v", len(out), err)
			}
			return out
		})
		boundedBytes, boundedAllocs := allocCost(func() []byte {
			out, err := Bounded(strings.NewReader(body), -1, 32<<20)
			if err != nil || len(out) != size {
				t.Fatalf("read = %d bytes, err %v", len(out), err)
			}
			return out
		})

		if boundedBytes > readAllBytes+readAllBytes/20 {
			t.Errorf("%d-byte body with no declared length: Bounded allocated %d bytes, io.ReadAll "+
				"allocated %d (%.2fx).\nThe undeclared-length path is the DOMINANT one on the AI "+
				"Gateway — the transport gunzips and reports ContentLength -1 — so a policy that is "+
				"cheaper only when a length is declared is a regression in production even when the "+
				"declared-length benchmark improves.",
				size, boundedBytes, readAllBytes, float64(boundedBytes)/float64(readAllBytes))
		}
		if boundedAllocs > readAllAllocs {
			t.Errorf("%d-byte body with no declared length: Bounded made %.1f allocations, io.ReadAll "+
				"made %.1f. Fewer, larger allocations are only worth having if the bytes come down "+
				"too; this asserts the count does not go the wrong way while the bytes stay level.",
				size, boundedAllocs, readAllAllocs)
		}
	}
}

// allocCost reports bytes and allocations per call. It measures rather than counts because the
// property under test is total bytes committed, which testing.AllocsPerRun does not report.
func allocCost(f func() []byte) (bytesPerOp uint64, allocsPerOp float64) {
	benchSink = f() // fault in anything the first call sets up
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const reps = 200
	for range reps {
		benchSink = f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / reps,
		float64(after.Mallocs-before.Mallocs) / reps
}

var benchSink []byte

// overDeliveringReader declares one length and hands over another. net/http frames both
// Request.Body and Response.Body by Content-Length, so a real wire cannot do this — but Bounded
// takes an io.Reader and documents the declared length as a HINT that correctness never depends
// on, and an infinite loop is a dependency.
type overDeliveringReader struct{ data []byte }

func (r *overDeliveringReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		// This is the shape that used to hang: an empty read window. Returning (0, nil) is what
		// strings.Reader and bytes.Reader do here, and it is what made the old loop spin.
		return 0, nil
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestBounded_SenderOverDeliversDoesNotHang is the regression test for a measured busy-loop.
// With declared == 100 and 5,000 bytes arriving, the old growth rule computed next = declared+1
// = 101 against a capacity of 101: the buffer did not grow, the read window became empty, and
// Read returned (0, nil) forever — 5 M iterations with len == cap == 101 and no progress. The
// fix is that growth must be strictly monotonic, so `next` falls back to doubling whenever the
// declared length does not exceed the current capacity.
//
// Run under a deadline so a regression reports THIS message instead of hanging the package's
// test binary until the go test timeout fires with no attribution.
func TestBounded_SenderOverDeliversDoesNotHang(t *testing.T) {
	body := strings.Repeat("x", 5000)

	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := Bounded(&overDeliveringReader{data: []byte(body)}, 100, 32<<20)
		done <- result{out, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("err = %v, want nil", got.err)
		}
		if string(got.out) != body {
			t.Fatalf("len = %d, want %d — over-delivered bytes must still be read in full",
				len(got.out), len(body))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Bounded did not return within 10s for a sender that delivered more than it " +
			"declared: growth is no longer strictly monotonic, so the read window is empty and " +
			"Read returns (0, nil) forever. This is a busy loop, not a slow path.")
	}
}

// TestBounded_DoublingIsClampedToTheCap covers the unknown-length growth path where doubling
// would overshoot maxBytes. The other truncation tests size the first allocation at or above the
// cap, so they return before ever growing and leave this arm unexercised.
func TestBounded_DoublingIsClampedToTheCap(t *testing.T) {
	const maxBytes = 700 // between the 512-byte first allocation and its 1,024-byte double
	body := strings.Repeat("d", 5000)

	got, err := Bounded(strings.NewReader(body), -1, maxBytes)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != maxBytes {
		t.Fatalf("len = %d, want %d", len(got), maxBytes)
	}
	if int64(cap(got)) > maxBytes {
		t.Fatalf("capacity = %d for a cap of %d. Capacity must never exceed maxBytes: the read "+
			"window is cap(buf) unclamped, so a capacity above the cap would read PAST it — and "+
			"the caller's cap is a DoS ceiling, not a suggestion.", cap(got), maxBytes)
	}
}

// TestBounded_CapacityNeverExceedsTheCap pins the invariant the unclamped read window rests on,
// across the shapes that decide the first allocation and the growth step. It is asserted
// separately because the window used to be clamped defensively at the read site; that clamp is
// gone now that the loop condition makes it unreachable, and this is what keeps it unreachable.
func TestBounded_CapacityNeverExceedsTheCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int64
		maxBytes int64
		bodyLen  int
	}{
		{"honest length under the cap", 200, 1000, 200},
		{"honest length equal to the cap", 1000, 1000, 1000},
		{"declared over the cap", 1 << 30, 300, 900},
		{"unknown length, body over the cap", -1, 300, 900},
		{"unknown length, tiny cap", -1, 10, 900},
		{"declared length under a tiny cap", 4, 10, 900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Bounded(strings.NewReader(strings.Repeat("c", tc.bodyLen)), tc.declared, tc.maxBytes)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if int64(len(got)) > tc.maxBytes {
				t.Fatalf("read %d bytes against a cap of %d", len(got), tc.maxBytes)
			}
			if int64(cap(got)) > tc.maxBytes {
				t.Fatalf("capacity %d exceeds the cap %d — the read window is cap(buf) unclamped, "+
					"so this would read past the caller's ceiling", cap(got), tc.maxBytes)
			}
		})
	}
}

// TestBounded_ExtremeDeclaredLengthDoesNotPanic is a confirmed regression, found by an
// adversarial review of this package on the day it landed. With declaredLen and maxBytes both
// math.MaxInt64 the old unconditional `initial++` wrapped to MinInt64, survived both clamps
// because a negative value is below them, and reached make([]byte, 0, negative):
// "makeslice: cap out of range". A Content-Length header alone triggered it.
//
// Reachability was narrow — every shipped cap is far below MaxInt64 (32 MiB on the AI Gateway's
// STT path, 10 MiB for payload capture), so an operator would have had to configure an int64-max
// cap — but this primitive sits on the macOS agent's host packet path, where a panic is not a
// failed request but a hole in the user's network, and CLAUDE.md requires that path to fail open.
// A guard is cheaper than the argument for leaving it out.
func TestBounded_ExtremeDeclaredLengthDoesNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int64
		maxBytes int64
	}{
		{"both at MaxInt64", math.MaxInt64, math.MaxInt64},
		{"declared MaxInt64, cap realistic", math.MaxInt64, 32 << 20},
		{"declared MaxInt64-1, cap MaxInt64", math.MaxInt64 - 1, math.MaxInt64},
		{"declared MaxInt64, cap MaxInt64-1", math.MaxInt64, math.MaxInt64 - 1},
		{"declared MinInt64", math.MinInt64, 1024},
		{"declared 1<<62, cap 1<<62", 1 << 62, 1 << 62},
		{"cap MaxInt64, length unknown", -1, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = "hello"
			got, err := Bounded(strings.NewReader(body), tc.declared, tc.maxBytes)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if string(got) != body {
				t.Fatalf("body = %q, want %q", got, body)
			}
			// The allocation must stay bounded by PreallocCap: an absurd declared length is a
			// claim, and honouring it would be the OOM vector this package exists to avoid.
			if int64(cap(got)) > PreallocCap {
				t.Fatalf("capacity %d for a %d-byte body declaring %d — an extreme declared length "+
					"must not size the buffer", cap(got), len(body), tc.declared)
			}
		})
	}
}

// stallingReader delivers n bytes and then stops, which is what a slowloris peer looks like:
// the declared length never arrives and the socket stays open.
type stallingReader struct{ left int }

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p)
	if n > r.left {
		n = r.left
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.left -= n
	return n, nil
}

// TestBounded_GrowthIsEarned_NoMemoryAmplification is the regression test for the defect an
// adversarial review found in the first version of this package: the growth step jumped straight
// to the declared length once PreallocCap bytes had arrived, so a peer that delivered 64 KiB + 1
// and then went quiet committed the WHOLE cap.
//
// Measured before the fix: 160x amplification at payload capture's default 10 MiB cap, 512x on
// the AI Gateway's 32 MiB STT path, 1600x at the admin API's 100 MiB ceiling — against
// io.ReadAll's 1.1x, so a 142x regression on the axis PreallocCap exists to protect. It reached
// the macOS agent's host packet path, and the bumped server sets no body-read deadline
// (ReadHeaderTimeout and IdleTimeout only), so the commitment lasts as long as the peer holds the
// socket open.
//
// The bound asserted is the one the algorithm now guarantees: capacity never exceeds twice what
// was delivered, plus the PreallocCap first allocation, which is unearned by design and bounded
// in absolute terms.
func TestBounded_GrowthIsEarned_NoMemoryAmplification(t *testing.T) {
	const delivered = (64 << 10) + 1 // one byte past PreallocCap, so growth must happen
	for _, tc := range []struct {
		name     string
		maxBytes int64
	}{
		{"payload-capture default 10MiB", 10 << 20},
		{"AI Gateway STT path 32MiB", 32 << 20},
		{"admin API ceiling 100MiB", 100 << 20},
		{"1GiB", 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The peer DECLARES the whole cap and delivers 64 KiB + 1.
			got, err := Bounded(&stallingReader{left: delivered}, tc.maxBytes, tc.maxBytes)
			if err == nil {
				t.Fatal("want the mid-body error, got nil")
			}
			if len(got) != delivered {
				t.Fatalf("len = %d, want %d", len(got), delivered)
			}
			bound := int64(2*delivered) + PreallocCap
			if int64(cap(got)) > bound {
				t.Fatalf("capacity %d for %d delivered bytes (%.1fx amplification), want at most %d.\n"+
					"A declared length must be earned geometrically: committing the whole cap after "+
					"PreallocCap bytes have arrived is a memory-amplification DoS, and this package "+
					"sits on the agent's host packet path where pinned memory is the user's machine.",
					cap(got), delivered, float64(cap(got))/float64(delivered), bound)
			}
		})
	}
}

// TestBounded_ExtremeDeclaredLengthInGrowthPath is the other half of the same review finding, and
// it exists because TestBounded_ExtremeDeclaredLengthDoesNotPanic above is BLIND to it: that test
// uses a 5-byte body, so len(buf) never reaches cap(buf) and the growth branch never executes. The
// first overflow fix guarded only the initial allocation, and the growth branch still handed the
// raw declared length to make() — `makeslice: cap out of range` at MaxInt64 and, worse,
// `fatal error: out of memory` around 1<<48, which is a runtime throw that recover() cannot catch.
func TestBounded_ExtremeDeclaredLengthInGrowthPath(t *testing.T) {
	body := strings.Repeat("y", (64<<10)+1) // past PreallocCap so growth is forced
	for _, tc := range []struct {
		name     string
		declared int64
		maxBytes int64
	}{
		{"both at MaxInt64", math.MaxInt64, math.MaxInt64},
		{"declared MaxInt64-1, cap MaxInt64", math.MaxInt64 - 1, math.MaxInt64},
		{"1<<62", 1 << 62, 1 << 62},
		{"1<<48", 1 << 48, 1 << 48},
		{"1<<40", 1 << 40, 1 << 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Bounded(strings.NewReader(body), tc.declared, tc.maxBytes)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if string(got) != body {
				t.Fatalf("len = %d, want %d", len(got), len(body))
			}
			// Twice the body plus the first allocation. Without the fix this was the declared
			// length itself: a 1 TiB capacity request for a 64 KiB body.
			bound := int64(2*len(body)) + PreallocCap
			if int64(cap(got)) > bound {
				t.Fatalf("capacity %d for a %d-byte body declaring %d, want at most %d",
					cap(got), len(body), tc.declared, bound)
			}
		})
	}
}

// TestBounded_HonestLargeBodyStillSizesExactly guards the win the growth rule must NOT lose: a
// body larger than PreallocCap, whose length was declared honestly, must not leave the caller
// holding a buffer materially wider than the body. Overshooting to the next geometric step was a
// measured +42 % regression when growth clamped only upward.
//
// The bound is len+1 rather than len exactly because both shapes are correct here: the chunk chain
// returns a joined slice sized exactly, while a body that fits the first chunk is returned as that
// chunk, whose capacity carries the deliberate spare byte that lets EOF land without a second
// allocation.
func TestBounded_HonestLargeBodyStillSizesExactly(t *testing.T) {
	const size = 400 << 10
	body := strings.Repeat("k", size)
	got, err := Bounded(strings.NewReader(body), int64(size), 32<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("len = %d, want %d", len(got), size)
	}
	if cap(got) > size+1 {
		t.Fatalf("capacity = %d for an honestly-declared %d-byte body, want at most %d. "+
			"Growth must land on the declared length rather than overshooting past it — that "+
			"overshoot was a measured +42 %% regression.", cap(got), size, size+1)
	}
}
