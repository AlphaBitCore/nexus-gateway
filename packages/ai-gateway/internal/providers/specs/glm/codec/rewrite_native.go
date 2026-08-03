package codec

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// RewriteNative delegates to the embedded empty-contract OpenAI identity
// codec: GLM is OpenAI-family (its native leg is the same-spec
// passthrough), so the family-wide wire behaviour — resolved-model stamp,
// streaming stream_options.include_usage injection — must be identical to
// every other OpenAI-chat sibling or GLM streams silently lose usage
// extraction. GLM-specific translation quirks stay in EncodeRequest,
// which only runs on the cross-format leg.
func (g GLMCodec) RewriteNative(shape typology.WireShape, nativeBody []byte, target provcore.CallTarget, stream bool) (provcore.EncodeResult, error) {
	return g.family.RewriteNative(shape, nativeBody, target, stream)
}
