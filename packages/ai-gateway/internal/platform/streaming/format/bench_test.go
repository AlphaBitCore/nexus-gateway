package format

import (
	"strings"
	"testing"
)

func BenchmarkSSEParser(b *testing.B) {
	// Build a realistic SSE stream with 100 chunks.
	var buf strings.Builder
	for range 100 {
		buf.WriteString(`data: {"choices":[{"delta":{"content":"Hello world, this is a benchmark test."}}]}`)
		buf.WriteString("\n\n")
	}
	buf.WriteString("data: [DONE]\n\n")
	input := buf.String()

	for b.Loop() {
		p := NewParser(strings.NewReader(input))
		for {
			evt, err := p.Next()
			if err != nil || evt.Done {
				break
			}
		}
	}
}

func BenchmarkExtractDeltaText(b *testing.B) {
	data := `{"choices":[{"delta":{"content":"Hello world, this is a benchmark."}}]}`
	for b.Loop() {
		ExtractDeltaText(data)
	}
}

func BenchmarkWriteEvent(b *testing.B) {
	data := `{"choices":[{"delta":{"content":"Hello"}}]}`
	var buf strings.Builder
	for b.Loop() {
		buf.Reset()
		_ = WriteEvent(&buf, data)
	}
}
