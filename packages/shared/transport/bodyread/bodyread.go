// Package bodyread buffers an HTTP body under a hard byte cap, sizing the buffer from a
// declared Content-Length when one is available and trustworthy.
//
// It exists because the same read shows up on three surfaces that must not drift apart:
// the AI Gateway's multimodal handlers, and the compliance-proxy / agent bumped path's
// request and response reads. The interaction between the declared length and the cap is
// subtle enough that two copies is the wrong number.
//
// Shape of the algorithm: a chain of separately allocated chunks, joined once into an
// exactly-sized result. Nothing is ever re-copied while reading, so the total allocated is
// (bytes delivered) + (chunk overshoot), where the overshoot is bounded by the geometric
// ladder below. The alternative — one buffer reallocated geometrically — costs roughly
// twice as much, because every growth step re-copies into a buffer that is itself
// oversized: doubling ends at up to 2x the body and the ladder sums to twice its own end,
// so ~4x the body against ~2.1x here. The trade is allocation COUNT, which the chunk chain
// does not improve on the unknown-length path; measurements are in the doc comment of
// readResponseBounded in the AI Gateway's ingress/proxy package.
//
// The alternative shape — a sync.Pool of buffers — was rejected for these call sites. A
// pool is cleared on every GC cycle, so on a low-rate endpoint the inter-arrival time can
// exceed the GC interval and most requests miss the pool, paying its bookkeeping for none
// of its benefit. Sizing has no hit-rate dependency, needs no ownership transfer to the
// audit record, and cannot pin an oversized buffer.
package bodyread

import (
	"errors"
	"io"
	"slices"
)

// PreallocCap bounds what a declared length alone may commit.
//
// A declared Content-Length is a CLAIM, not evidence. Anything upstream that can set the
// header could declare the full cap — 32 MiB on the AI Gateway's STT path — and then send
// one byte; N concurrent such responses would commit N x 32 MiB before a single payload
// byte arrived. Growth driven by delivered bytes never had that exposure.
//
// So the header sizes the FIRST chunk only up to this ceiling. Past it, growth must be
// earned: every later chunk is allocated only after the previous one was filled. 64 KiB
// covers the bodies these call sites actually see — a chat completion, an STT transcript,
// a video job descriptor, a provider error body — in a single allocation.
const PreallocCap = 64 << 10

// unknownFirstChunk is the first allocation when no usable length was declared. It is what
// io.ReadAll starts with, so a body that fits inside it costs the same either way.
const unknownFirstChunk = 512

// ladderFloor is the smallest follow-on chunk. The ladder is seeded from half the first
// chunk, which for the unknown-length path lands exactly on io.ReadAll's own follow-on
// ladder (256, then x1.5). Without a floor, a tiny declared length that the sender then
// over-delivers would walk the rest of the body in single-digit-byte chunks.
const ladderFloor = 256

// Bounded reads at most maxBytes from src, returning what it read.
//
// declaredLen is the sender's claimed body length (http.Request.ContentLength or
// http.Response.ContentLength), or a negative value when unknown (chunked, or a response
// the transport transparently decompressed). It is a sizing HINT only; correctness never
// depends on it, and PreallocCap bounds how much of it may be trusted before bytes arrive.
//
// Overflow past maxBytes is TRUNCATED, not an error: the partial body is still useful for
// usage extraction and capture, and the rest of the stream is abandoned along with the
// connection. Callers that need to distinguish a truncated body from a complete one must
// compare len(out) against maxBytes themselves — this mirrors what the io.ReadAll callers
// it replaces already did.
//
// A nil src or a non-positive maxBytes reads nothing. Bounded never closes src; every
// caller has its own close discipline and keeps it.
func Bounded(src io.Reader, declaredLen, maxBytes int64) ([]byte, error) {
	if src == nil || maxBytes <= 0 {
		return nil, nil
	}

	// declared is the length we are willing to size toward. Unknown, empty, or over-cap
	// lengths contribute nothing: an over-cap declaration must not widen anything, because
	// the cap is the ceiling regardless of what was claimed.
	declared := declaredLen
	if declared < 0 || declared > maxBytes {
		declared = 0
	}

	// The spare +1 is on purpose. Sized to exactly `declared`, a well-behaved sender fills
	// the chunk, the loop allocates a second one BEFORE discovering EOF, and the result then
	// needs a join it would not otherwise need. One spare byte lets the EOF land inside the
	// first chunk, so an honest length costs a single allocation and no copy.
	//
	// It is applied only in the branch where it CANNOT overflow. An unconditional increment
	// wraps to a negative value when declaredLen and maxBytes are both math.MaxInt64, and a
	// negative value survives the clamps below and reaches make([]byte, negative) —
	// "makeslice: cap out of range", reachable from a Content-Length header alone. Ordering
	// the PreallocCap clamp first makes that unreachable by construction.
	size := declared
	switch {
	case size <= 0:
		size = unknownFirstChunk
	case size > PreallocCap:
		size = PreallocCap // not an exact fit, so no spare byte is wanted
	default:
		size++ // <= PreallocCap, so this cannot overflow
	}

	// ladder is the size of the chunk AFTER the current one, growing by 1.5x per step. Half
	// the first chunk is the seed, which reproduces io.ReadAll's ladder on the unknown-length
	// path and keeps the follow-on chunks proportionate to a large first chunk on the
	// declared path. A 1.5x ladder wastes at most a third of its last chunk; doubling would
	// waste up to half, and that waste is the whole cost difference against io.ReadAll.
	ladder := size / 2
	if ladder < ladderFloor {
		ladder = ladderFloor
	}

	// first holds chunk 0, rest holds the others. Splitting them keeps the common case — a
	// body that fits the first chunk — at exactly one allocation, with no slice header block
	// and no join.
	//
	// rest starts on a fixed-size array so the chunk list itself costs nothing: it does not
	// escape (join copies out of it and keeps nothing), so it stays on the stack until the
	// ladder needs a ninth chunk, which for the unknown-length path is past 100 KiB of body.
	var first []byte
	var restArr [8][]byte
	rest := restArr[:0]
	var total int64

	for {
		// The cap is the ceiling on every allocation, so a chunk never reaches past it and
		// total can never exceed it.
		if room := maxBytes - total; size > room {
			size = room
		}
		// An honest declared length caps the remaining work exactly, which is what stops a
		// 400 KiB body from overshooting into a 165 KiB final chunk. `want > 0` keeps the
		// clamp off the over-delivery path: a sender that hands over more than it declared
		// must still make progress, and a zero-length chunk would spin forever.
		if declared > 0 {
			if want := declared + 1 - total; want > 0 && want < size {
				size = want
			}
		}

		var chunk []byte
		if first == nil {
			// The first chunk is sized deliberately — from the declared length, or from the
			// unknown-length default — and a caller that declared honestly gets this slice
			// back verbatim. Rounding it up would hand that caller a wider buffer than the
			// body it asked for, so this one is allocated exactly.
			chunk = make([]byte, size)
		} else {
			// Every allocation is rounded up to a size class by the runtime, and a plain make
			// hides that slack behind a capacity equal to the length. Asking through append
			// exposes it, and using it is free: those bytes are already committed. It is worth
			// the idiom — throwing the slack away costs a whole extra chunk on bodies that land
			// just past a ladder step.
			chunk = slices.Grow([]byte(nil), int(size))
			chunk = chunk[:min(int64(cap(chunk)), maxBytes-total)]
		}
		n, err := fill(src, chunk)
		total += int64(n)
		if first == nil {
			first = chunk[:n]
		} else if n > 0 {
			rest = append(rest, chunk[:n])
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return join(first, rest, total), err
		}
		if total >= maxBytes {
			return join(first, rest, total), nil
		}

		size = ladder
		// The /2 test removes any chance of int64 overflow; a ladder at the cap is clamped
		// back to the remaining room at the top of the next iteration anyway.
		if ladder <= maxBytes/2 {
			ladder += ladder / 2
		} else {
			ladder = maxBytes
		}
	}
}

// fill reads until chunk is full or src reports anything. A short read is not an ending —
// a network body arrives in record-sized pieces — so only an error or a full chunk stops
// it, which is the same rule the single-buffer loop it replaces followed.
func fill(src io.Reader, chunk []byte) (int, error) {
	filled := 0
	for filled < len(chunk) {
		n, err := src.Read(chunk[filled:])
		filled += n
		if err != nil {
			return filled, err
		}
	}
	return filled, nil
}

// join concatenates the chunk chain into one exactly-sized slice. When everything landed in
// the first chunk there is nothing to concatenate, and returning it directly is what keeps
// an honestly-declared body at one allocation and zero copies.
func join(first []byte, rest [][]byte, total int64) []byte {
	if len(rest) == 0 {
		return first
	}
	out := make([]byte, 0, total)
	out = append(out, first...)
	for _, c := range rest {
		out = append(out, c...)
	}
	return out
}
