package codec

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// RewriteNative is verbatim for the Gemini wire: the model travels in the
// URL path (Transport.BuildURL renders it from CallTarget.ProviderModelID),
// never in the body, so a same-spec body has no model stamp due and no
// evidence-cited per-model body quirk exists on this wire today.
func (Codec) RewriteNative(_ typology.WireShape, nativeBody []byte, _ provcore.CallTarget, _ bool) (provcore.EncodeResult, error) {
	return provcore.EncodeResult{Body: nativeBody, ContentType: "application/json"}, nil
}
