package bedrock

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// RewriteNative is verbatim for the Bedrock wire: the model id is encoded
// into the invoke URL (this codec deletes it from the body on the encode
// side), so a same-spec body has no model stamp due and no evidence-cited
// per-model body quirk exists on this wire today.
func (c codec) RewriteNative(_ typology.WireShape, nativeBody []byte, _ provcore.CallTarget, _ bool) (provcore.EncodeResult, error) {
	return provcore.EncodeResult{Body: nativeBody, ContentType: "application/json"}, nil
}
