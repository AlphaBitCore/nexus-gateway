// stream_context.go — the per-stream state carrier and constructor for
// the streaming stage chain. handleStreamWithSubscription
// (proxy_responses.go) drives the chain: preamble → response hooks →
// wire shape → relay → accounting; each stage is a type in its
// stream_<name>.go file. This is the streaming sibling of the
// request-level chain in stage_context.go — it reuses the same
// proxyStage interface so both drivers share one stage vocabulary.
package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	streamcache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// streamState carries the per-stream state shared across the streaming
// stage chain. Field groups are owned by the stage that produces them;
// later stages only read.
type streamState struct {
	h *Handler
	w http.ResponseWriter
	r *http.Request

	rec     *audit.Record
	sub     streamcache.ChunkSubscription
	target  routingcore.RoutingTarget
	coerced []string

	quotaInPrice  float64
	quotaOutPrice float64
	quotaDecision *quota.Decision

	endpointType string
	requestID    string
	start        time.Time
	logger       *slog.Logger

	// Response-hooks outputs (stream_hooks.go): the per-checkpoint
	// pipeline runner and whether assistant deltas are held back until
	// the first compliance checkpoint approves.
	hookRunner func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult
	holdBack   bool

	// responseHooksActive is false only when the stream-entry probe proved
	// there are NO response-stage rules wired. The live pipeline uses it to
	// skip installing the per-checkpoint Registry-normalize PreHook (and the
	// raw-accumulating TeeReader it requires): with no response hook to consume
	// the normalized payload, that work is pure waste. Same probe + same
	// staleness window as the holdBack decision.
	responseHooksActive bool

	// Wire-shape outputs (stream_shape.go): the `[DONE]` sentinel
	// decision, the admin streaming mode + buffer cap, and the
	// cross-format / cross-ingress transcoder selection.
	emitDone             bool
	streamMode           streampolicy.Mode
	streamMaxBufferBytes int
	transcoder           canonicalbridge.StreamTranscoder
	ingressFormat        provcore.Format

	// Relay outputs (stream_relay.go), read by the accounting stage:
	// the SSE reader (terminal-error classification) and the usage
	// holder (final reported usage observed in the chunk timeline).
	sseReader   *chunkSSEReader
	usageHolder *chunkUsageHolder
}

// newStreamState builds the carrier from the driver's arguments. All
// derived state is produced by the stages themselves.
func (h *Handler) newStreamState(
	r *http.Request,
	w http.ResponseWriter,
	rec *audit.Record,
	sub streamcache.ChunkSubscription,
	target routingcore.RoutingTarget,
	coerced []string,
	quotaInPrice, quotaOutPrice float64,
	quotaDecision *quota.Decision,
	endpointType, requestID string,
	start time.Time,
	logger *slog.Logger,
) *streamState {
	return &streamState{
		h:             h,
		w:             w,
		r:             r,
		rec:           rec,
		sub:           sub,
		target:        target,
		coerced:       coerced,
		quotaInPrice:  quotaInPrice,
		quotaOutPrice: quotaOutPrice,
		quotaDecision: quotaDecision,
		endpointType:  endpointType,
		requestID:     requestID,
		start:         start,
		logger:        logger,
	}
}
