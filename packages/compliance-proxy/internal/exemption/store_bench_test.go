package exemption

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

// IsExempt is called on EVERY bumped request (tlsbump forward path, via
// compliance-proxy/internal/proxy/forward). The overwhelmingly common
// deployment state is ZERO temporary exemptions — they are a break-glass
// tool, granted rarely and expiring on their own — so the empty-store path is
// the one that actually runs in production.

func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// populate adds n live exemptions that deliberately do NOT match the probe, so
// the lookup walks the whole set — the worst case for a linear scan.
func populate(tb testing.TB, s *Store, n int) {
	tb.Helper()
	for i := range n {
		if e := s.Add(fmt.Sprintf("10.9.%d.%d", i/256, i%256), fmt.Sprintf("host-%d.example.com", i),
			time.Hour, "bench", "bench"); e == nil {
			tb.Fatal("Add returned nil — the benchmark would measure an empty store")
		}
	}
}

// BenchmarkIsExempt_EmptyStore is the production-common case: no exemptions
// granted. Everything measured here is pure overhead on every request.
func BenchmarkIsExempt_EmptyStore(b *testing.B) {
	s := NewStore(benchLogger())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if ok, _ := s.IsExempt("192.168.1.10", "api.openai.com"); ok {
			b.Fatal("empty store must not report an exemption")
		}
	}
}

// BenchmarkIsExempt_NoMatch_8 / _64 show how the linear scan scales when
// exemptions ARE present but none match the request.
func BenchmarkIsExempt_NoMatch_8(b *testing.B)  { benchNoMatch(b, 8) }
func BenchmarkIsExempt_NoMatch_64(b *testing.B) { benchNoMatch(b, 64) }

func benchNoMatch(b *testing.B, n int) {
	s := NewStore(benchLogger())
	populate(b, s, n)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if ok, _ := s.IsExempt("192.168.1.10", "api.openai.com"); ok {
			b.Fatal("unexpected match")
		}
	}
}
