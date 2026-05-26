package canonicalbridge

import (
	"context"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// StreamTranscoder converts a canonical provider chunk stream into the wire
// SSE frames an ingress client expects. Implementations are registered per
// (ingress, target) pair; [StreamShapeCompatible] is the conservative guard.
//
// Invariants: Write must not mutate [provcore.Chunk] arguments. When chunk.Done
// is true, the transcoder appends the ingress-specific end-of-stream marker
// (for example OpenAI `data: [DONE]\n\n`, Anthropic `event: message_stop`).
type StreamTranscoder interface {
	Write(ctx context.Context, chunk provcore.Chunk) ([]byte, error)
}
