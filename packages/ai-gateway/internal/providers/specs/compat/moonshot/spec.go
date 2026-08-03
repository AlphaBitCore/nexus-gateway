// Package spec_moonshot wires the Moonshot Kimi provider AdapterSpec.
// Moonshot exposes an OpenAI-compatible chat completions API at
// api.moonshot.cn/v1/chat/completions (CN region) and
// api.moonshot.ai/v1/chat/completions (international region); this
// codec reuses every spec_openai component and only changes the
// [provcore.Format] tag so vendor audit / metrics / policy can
// target Moonshot specifically.
package moonshot

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Moonshot [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:    provcore.FormatMoonshot,
		Transport: openai.NewTransport(log),
		// The contract carries the kimi fixed-temp strips into both codec
		// entry points — the cross-format canonical door (bodies bridged
		// from /v1/messages and the other non-OpenAI ingresses) and the
		// native-leg differential. No dispatch-level rewrite callback.
		SchemaCodec:     openai.NewIdentityCodec(Contract()),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
