package bodyread

import (
	"bytes"
	"io"
	"runtime"
	"sync"
	"testing"
)

// The pooled alternative this package's doc comment rejects, measured rather than
// asserted.
//
// Cost unit: ONE bumped request per iteration — the compliance-proxy calls Bounded
// twice per intercepted exchange (request body, response body), and a heap profile of
// the whole bumped handler attributes 15.4 % of its allocated bytes to these two calls,
// the largest poolable buffer on that path.
//
// Two regimes are measured because they give opposite answers and a tight loop only
// shows one of them:
//
//   - _Hot: back-to-back iterations, no GC between them. Every Get hits. This is the
//     best case a pool can ever have and is what an unqualified "pooling wins" claim
//     is actually measuring.
//   - _AfterGC: runtime.GC() between iterations. sync.Pool is cleared on every GC
//     cycle, so every Get misses and pays New plus the Get/Put bookkeeping. This is
//     the regime a proxy sees whenever request inter-arrival time on a given P exceeds
//     the GC interval.
//
// A pooled Bounded would additionally need an ownership transfer, because the body it
// returns is aliased by the audit event and outlives the request. That cost is not in
// these numbers — these are the pool's CEILING, measured without the machinery that
// would be required to make it correct.

// benchPool is the pooled shape the rejected design would use: a handle pool in the
// ai-gateway idiom (*[]byte so Put takes no allocation), pre-grown to PreallocCap.
var benchPool = sync.Pool{New: func() any { b := make([]byte, 0, PreallocCap); return &b }}

// boundedPooled is Bounded with its first buffer taken from benchPool instead of
// sized by make. Growth and the read loop are identical, so the delta between the two
// arms is exactly "where did the first buffer come from".
//
// It takes no declared length, and that is the finding rather than a simplification:
// once the buffer comes from a pool the declared Content-Length has nothing left to
// size, so the allocation on a miss is the pool's class (PreallocCap) whatever the
// body's real length is. That is why the miss regime below costs more for a 512 B
// body than sizing costs for a 64 KiB one.
func boundedPooled(src io.Reader, maxBytes int64) ([]byte, *[]byte, error) {
	if src == nil || maxBytes <= 0 {
		return nil, nil, nil
	}
	hp := benchPool.Get().(*[]byte)
	buf := (*hp)[:0]
	for int64(len(buf)) < maxBytes {
		if len(buf) == cap(buf) {
			next := int64(cap(buf))
			if next <= maxBytes/2 {
				next *= 2
			} else {
				next = maxBytes
			}
			grown := make([]byte, len(buf), next)
			copy(grown, buf)
			buf = grown
		}
		n, err := src.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			*hp = buf
			if err == io.EOF {
				err = nil
			}
			return buf, hp, err
		}
	}
	*hp = buf
	return buf, hp, nil
}

func releaseBenchPool(hp *[]byte) {
	if hp != nil && cap(*hp) <= 2<<20 {
		benchPool.Put(hp)
	}
}

var benchOut []byte

// benchBodies are the sizes an intercepted LLM exchange actually carries: a short
// tool-call turn, a chat request, and a long-prompt request.
var benchBodies = map[string]int{"512B": 512, "8KiB": 8 << 10, "64KiB": 64 << 10}

func runSized(b *testing.B, size int, gc bool) {
	b.Helper()
	body := bytes.Repeat([]byte("x"), size)
	b.ReportAllocs()
	for range b.N {
		out, err := Bounded(bytes.NewReader(body), int64(size), 10<<20)
		if err != nil || len(out) != size {
			b.Fatalf("Bounded = %d bytes, err %v", len(out), err)
		}
		benchOut = out
		if gc {
			runtime.GC()
		}
	}
}

func runPooled(b *testing.B, size int, gc bool) {
	b.Helper()
	body := bytes.Repeat([]byte("x"), size)
	b.ReportAllocs()
	for range b.N {
		out, hp, err := boundedPooled(bytes.NewReader(body), 10<<20)
		if err != nil || len(out) != size {
			b.Fatalf("boundedPooled = %d bytes, err %v", len(out), err)
		}
		benchOut = out
		releaseBenchPool(hp)
		if gc {
			runtime.GC()
		}
	}
}

func BenchmarkBodyRead_Sized_Hot(b *testing.B) {
	for name, size := range benchBodies {
		b.Run(name, func(b *testing.B) { runSized(b, size, false) })
	}
}

func BenchmarkBodyRead_Pooled_Hot(b *testing.B) {
	for name, size := range benchBodies {
		b.Run(name, func(b *testing.B) { runPooled(b, size, false) })
	}
}

func BenchmarkBodyRead_Sized_AfterGC(b *testing.B) {
	for name, size := range benchBodies {
		b.Run(name, func(b *testing.B) { runSized(b, size, true) })
	}
}

func BenchmarkBodyRead_Pooled_AfterGC(b *testing.B) {
	for name, size := range benchBodies {
		b.Run(name, func(b *testing.B) { runPooled(b, size, true) })
	}
}
