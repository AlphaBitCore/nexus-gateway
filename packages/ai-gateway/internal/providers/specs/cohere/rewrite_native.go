package cohere

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// RewriteNative is verbatim for the Cohere chat / embed wires (no aliasing
// path targets them natively and no evidence-cited per-model quirk exists —
// §3a Rule 7 forbids a speculative one). The rerank wire is the exception:
// its canonical shape IS the Cohere shape, so a same-format rerank request
// reaches Cohere natively, and the client-facing alias / routing-changed
// model in the body root must be stamped to the resolved provider model or
// Cohere 404s (the model-in-body rewrite the old passthrough flag performed).
func (codec) RewriteNative(shape typology.WireShape, nativeBody []byte, target provcore.CallTarget, _ bool) (provcore.EncodeResult, error) {
	if shape == typology.WireShapeCohereRerank && target.ProviderModelID != "" {
		out, err := provcore.SurgicalModelStamp(nativeBody, target.ProviderModelID)
		if err != nil {
			return provcore.EncodeResult{}, err
		}
		return provcore.EncodeResult{Body: out, ContentType: "application/json"}, nil
	}
	return provcore.EncodeResult{Body: nativeBody, ContentType: "application/json"}, nil
}
