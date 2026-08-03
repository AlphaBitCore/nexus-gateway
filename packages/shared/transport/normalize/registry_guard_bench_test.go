package normalize

import (
	"context"
	"strings"
	"testing"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The C-32 guard runs on EVERY Registry.Normalize call, so its cost claim ("two byte
// comparisons") has to be measured rather than asserted — that is what B2 is for, and
// asserting a cost from code shape is the specific error this program has made four times.
//
// These arms drive the real Registry through the real codec chain, so the guard's cost is
// measured where it actually runs rather than in isolation.

func guardBenchBody(messages int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-4o","messages":[`)
	for i := range messages {
		if i > 0 {
			b.WriteByte(',')
		}
		// Escaped quotes and backslashes throughout: ordinary chat content, and the shape
		// where a naive "any backslash" gate would have rejected real traffic.
		b.WriteString(`{"role":"user","content":"he said \"hi\" about c:\\tmp\\logs and more text"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func benchNormalize(b *testing.B, messages int) {
	reg := BuildRegistry()
	body := guardBenchBody(messages)
	meta := normcore.Meta{
		AdapterType: "openai", Direction: normcore.DirectionRequest,
		ContentType: "application/json", EndpointPath: "/v1/chat/completions",
	}
	ctx := context.Background()

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for range b.N {
		p, err := reg.Normalize(ctx, body, meta)
		if err != nil {
			b.Fatalf("Normalize: %v", err)
		}
		guardBenchSink = p.Kind
	}
}

var guardBenchSink normcore.Kind

func BenchmarkRegistryNormalize_Small(b *testing.B)  { benchNormalize(b, 1) }
func BenchmarkRegistryNormalize_Medium(b *testing.B) { benchNormalize(b, 40) }
func BenchmarkRegistryNormalize_Large(b *testing.B)  { benchNormalize(b, 800) }
