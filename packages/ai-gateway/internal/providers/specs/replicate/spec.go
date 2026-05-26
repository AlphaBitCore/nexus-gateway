package replicate

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// NewSpec returns the Replicate [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatReplicate,
		Transport:       NewTransport(log),
		SchemaCodec:     codec{},
		StreamDecoder:   NewStreamDecoder(log),
		ErrorNormalizer: errorNormalizer{},
	}
}
