package anthropic_test

import (
	"encoding/base64"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Anchors the cost of document translation on the three shapes that matter, so
// a claim about it is a measurement rather than an adjective.
//
// NoAttachment is the one to watch. It is the overwhelming majority of traffic
// and none of the document work is for it — any movement there is a regression,
// however small.
func benchBody(part string) []byte {
	return []byte(`{"model":"claude","max_tokens":64,"messages":[
	    {"role":"system","content":"you are a helpful assistant"},
	    {"role":"user","content":[` + part + `{"type":"text","text":"what is the reference number?"}]}]}`)
}

func filePart(mediaType string, size int) string {
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("reference 52903\n", size/16)))
	return `{"type":"file","file":{"filename":"doc","file_data":"data:` + mediaType +
		`;base64,` + data + `"}},`
}

func benchEncode(b *testing.B, body []byte) {
	b.Helper()
	spec := anthropic.NewSpec(nil)
	tgt := provcore.CallTarget{ProviderModelID: "claude-sonnet-4"}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		if _, err := spec.SchemaCodec.EncodeRequest(
			typology.WireShapeAnthropicMessages, body, tgt); err != nil {
			b.Fatalf("encode: %v", err)
		}
	}
}

func BenchmarkEncode_NoAttachment(b *testing.B) {
	benchEncode(b, benchBody(""))
}

func BenchmarkEncode_PDF_64KiB(b *testing.B) {
	benchEncode(b, benchBody(filePart("application/pdf", 64*1024)))
}

func BenchmarkEncode_TextDocument_64KiB(b *testing.B) {
	benchEncode(b, benchBody(filePart("text/markdown", 64*1024)))
}
