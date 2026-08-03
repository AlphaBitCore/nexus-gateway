package voyage

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// RewriteNative is verbatim for the Voyage embeddings wire today: no
// aliasing/rewrite path targets it natively and no evidence-cited per-model
// quirk exists (§3a Rule 7 forbids a speculative one). Revisit with the
// first observed vendor rejection.
func (codec) RewriteNative(_ typology.WireShape, nativeBody []byte, _ provcore.CallTarget, _ bool) (provcore.EncodeResult, error) {
	return provcore.EncodeResult{Body: nativeBody, ContentType: "application/json"}, nil
}
