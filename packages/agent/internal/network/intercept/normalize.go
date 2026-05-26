package intercept

import (
	"context"
	"errors"
	"log/slog"

	hookscore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// preferAdapterNormalize bridges the agent's hot path to the unified
// adapter.Normalize interface. When the matched
// adapter implements normalize.Normalizer (every consumer-surface
// adapter does as of S12), Normalize produces a structured
// NormalizedPayload with role-aware Messages — PII / DLP hooks then
// distinguish user prompt vs assistant reply without flattening
// segments. Falls back to the legacy NormalizedContent.Segments path
// when:
//
//   - the adapter doesn't implement Normalizer (cursor's
//     gRPC-protobuf adapter is the open case),
//   - Normalize returned ErrUnsupported (probe confidence below the
//     per-adapter threshold — usually a non-AI endpoint on the same
//     host such as auth / settings paths the adapter doesn't claim
//     a chat shape on),
//   - or Normalize returned a hard error (logged at Warn).
//
// nc.Segments is supplied as the fallback projection so the caller
// doesn't need to re-call ExtractRequest/Response.
func preferAdapterNormalize(
	ctx context.Context,
	adapter traffic.Adapter,
	body []byte,
	path string,
	direction normalizecore.Direction,
	fallbackSegments []string,
	logger *slog.Logger,
) *normalizecore.NormalizedPayload {
	if adapter != nil && len(body) > 0 {
		if n, ok := adapter.(normalizecore.Normalizer); ok {
			payload, err := n.Normalize(ctx, body, normalizecore.Meta{
				AdapterType:  adapter.ID(),
				Direction:    direction,
				EndpointPath: path,
			})
			if err == nil {
				return &payload
			}
			if !errors.Is(err, normalizecore.ErrUnsupported) {
				logger.Warn("adapter.Normalize failed; falling back to legacy segments",
					slog.String("adapter", adapter.ID()),
					slog.String("direction", string(direction)),
					slog.String("error", err.Error()),
				)
			}
		}
	}
	if len(fallbackSegments) == 0 {
		return nil
	}
	return hookscore.PayloadFromTextSegments(fallbackSegments)
}
