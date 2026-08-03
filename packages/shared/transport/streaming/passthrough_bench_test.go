package streaming

import (
	"bytes"
	"context"
	"testing"
)

// Passthrough is the relay used for every SSE response that is NOT inspected —
// kill-switch, pinning exemption, PASSTHROUGH path action, attested traffic, and
// the non-enforcing streaming modes. It is therefore the highest-volume relay on
// compliance-proxy and agent, and its per-stream read buffer is paid once per
// stream regardless of how short the stream is.

func benchPassthrough(b *testing.B, fixture []byte) {
	ctx := context.Background()
	b.SetBytes(int64(len(fixture)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var out bytes.Buffer
		if err := Passthrough(ctx, bytes.NewReader(fixture), &out); err != nil {
			b.Fatalf("Passthrough: %v", err)
		}
		if out.Len() != len(fixture) {
			b.Fatalf("relay lost bytes: got %d want %d", out.Len(), len(fixture))
		}
	}
}

// BenchmarkPassthrough_ShortStream isolates the per-STREAM buffer cost: with a
// tiny payload, almost all the allocation is the relay's own read buffer. A
// proxy serving many short exchanges pays this repeatedly.
func BenchmarkPassthrough_ShortStream(b *testing.B) { benchPassthrough(b, sseFixture(3)) }

// BenchmarkPassthrough_TypicalStream is a normal reply.
func BenchmarkPassthrough_TypicalStream(b *testing.B) { benchPassthrough(b, sseFixture(150)) }

// BenchmarkPassthrough_Parallel models real proxy concurrency, where a per-call
// allocation turns into sustained GC pressure rather than a one-off cost.
func BenchmarkPassthrough_Parallel(b *testing.B) {
	fixture := sseFixture(20)
	ctx := context.Background()
	b.SetBytes(int64(len(fixture)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var out bytes.Buffer
			if err := Passthrough(ctx, bytes.NewReader(fixture), &out); err != nil {
				b.Fatalf("Passthrough: %v", err)
			}
		}
	})
}
