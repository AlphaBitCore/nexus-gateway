package audit

import (
	"bytes"
	"math/rand"
	"testing"
)

// sized50k mimics the loadtest's ~50KB space-separated-words prompt body.
func sized50k() []byte {
	words := []string{"system", "latency", "throughput", "token", "cache", "stream", "gateway", "request", "model", "context", "budget", "concurrency", "scale", "quota"}
	r := rand.New(rand.NewSource(1))
	var sb bytes.Buffer
	sb.WriteString(`{"model":"mock-gpt-4o","messages":[{"role":"user","content":"`)
	for sb.Len() < 50000 {
		sb.WriteString(words[r.Intn(len(words))])
		sb.WriteByte(' ')
	}
	sb.WriteString(`"}]}`)
	return sb.Bytes()
}

func BenchmarkCompress_Zstd(b *testing.B) {
	src := sized50k()
	b.SetBytes(int64(len(src)))
	var out []byte
	b.ResetTimer()
	for range b.N {
		out = compressInlineToBase64(out[:0], src)
	}
	b.ReportMetric(float64(len(out))/float64(len(src))*100, "%ratio")
}

func BenchmarkCompress_S2(b *testing.B) {
	src := sized50k()
	b.SetBytes(int64(len(src)))
	var out []byte
	b.ResetTimer()
	for range b.N {
		out = compressInlineS2ToBase64(out[:0], src)
	}
	b.ReportMetric(float64(len(out))/float64(len(src))*100, "%ratio")
}
