package core

import (
	"fmt"
	"testing"
)

// BenchmarkCompilePattern_ColdCold measures per-call cost when the cache
// capacity is 1 so every Get() misses and every call recompiles.
func BenchmarkCompilePattern_ColdCold(b *testing.B) {
	b.Cleanup(func() { SetRegexCacheCap(10000) })
	patterns := make([]string, 500)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`(?i)\b(example-%d)\b`, i)
	}
	SetRegexCacheCap(1)
	b.ResetTimer()
	for range b.N {
		for _, p := range patterns {
			if _, err := CompilePattern(p, ""); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkCompilePattern_Warm measures per-call cost after the cache is
// primed so every Get() is a hit.
func BenchmarkCompilePattern_Warm(b *testing.B) {
	b.Cleanup(func() { SetRegexCacheCap(10000) })
	patterns := make([]string, 500)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`(?i)\b(warm-%d)\b`, i)
	}
	SetRegexCacheCap(10000)
	for _, p := range patterns {
		_, _ = CompilePattern(p, "") // prime
	}
	b.ResetTimer()
	for range b.N {
		for _, p := range patterns {
			_, _ = CompilePattern(p, "")
		}
	}
}
