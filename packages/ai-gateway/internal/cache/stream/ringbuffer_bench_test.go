package streamcache

import (
	"context"
	"errors"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

var errBenchStop = errors.New("bench stop")

// benchReadPark drives one Append per parked subscriber round trip, which is
// the live-stream shape: subscribers drain faster than the upstream produces,
// so every chunk costs one park and one wake per subscriber. It is a
// regression guard for the allocation count of that park.
func benchReadPark(b *testing.B, subscribers int) {
	b.Helper()
	rb := NewRingBuffer()
	consumed := make(chan struct{}, subscribers)
	stopped := make(chan struct{}, subscribers)

	for range subscribers {
		go func() {
			idx := 0
			for {
				var err error
				_, idx, err = rb.Read(context.Background(), idx)
				if err != nil {
					stopped <- struct{}{}
					return
				}
				consumed <- struct{}{}
			}
		}()
	}

	// The buffer keeps every chunk for the replay window, so over b.N
	// iterations the chunk slice's own growth would dominate the allocation
	// counters and hide what is being measured. Reserving it up front leaves
	// only the park/wake machinery inside the timed region; in production that
	// growth is amortised and identical either way.
	rb.mu.Lock()
	rb.chunks = make([]provcore.Chunk, 0, b.N+1)
	rb.mu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rb.Append(provcore.Chunk{})
		for range subscribers {
			<-consumed
		}
	}
	b.StopTimer()

	rb.Fail(errBenchStop)
	for range subscribers {
		<-stopped
	}
}

func BenchmarkRingBufferReadPark1(b *testing.B) { benchReadPark(b, 1) }
func BenchmarkRingBufferReadPark4(b *testing.B) { benchReadPark(b, 4) }
