// stream_shape.go — the wire-shape stage of the streaming stage chain:
// the OpenAI `[DONE]` sentinel decision, the admin streaming mode +
// buffer cap, and the cross-format / cross-ingress transcoder
// selection. Owns streamState.emitDone / streamMode /
// streamMaxBufferBytes / transcoder / ingressFormat.
package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// streamShapeStage resolves how chunks are encoded for this client.
type streamShapeStage struct{ s *streamState }

func (st streamShapeStage) run() bool {
	s := st.s
	h := s.h
	r := s.r
	target := s.target

	// The OpenAI `[DONE]` terminator is conditional on the ingress
	// format: OpenAI-shape clients expect it as the stream sentinel,
	// Anthropic / Gemini clients do NOT (their typed terminal event
	// closes the stream and a stray `data: [DONE]` line confuses
	// strict SDK parsers — Claude Code's symptom of "blank assistant
	// message even though all deltas arrived" was this exact bug).
	emitDone := false
	if ingress, ok := IngressFromContext(r.Context()); ok {
		emitDone = ingress.BodyFormat.IsOpenAIFamily()
	}
	// Resolve admin streaming mode + buffer cap from the Store.
	// ai-gateway honors buffer_full_block (replaces the prior
	// "chunked_async only" hardcode). Three-service
	// alignment: agent / compliance-proxy / ai-gateway all dispatch on
	// the same streampolicy.Store snapshot. Nil Store falls through to
	// chunked_async — the default for traffic that has already opted
	// into the gateway (unlike tlsbump's transparent-forwarder posture
	// where nil Store means "no opt-in, transparent passthrough").
	//
	// Read MaxBufferBytes from the same snapshot so
	// admin-configured caps (64MB default, larger for high-volume
	// deployments) propagate into both buffer and live pipelines. Zero
	// means "use the pipeline's built-in default" (8MB) — same shape as
	// the underlying BufferConfig / LiveConfig.
	streamMode := streampolicy.ModeChunkedAsync
	streamMaxBufferBytes := 0
	if h.deps.StreamingPolicy != nil {
		snapshot := h.deps.StreamingPolicy.Get()
		streamMode = snapshot.Mode
		streamMaxBufferBytes = snapshot.MaxBufferBytes
	}

	// Enforcement routing (B2): an enforcing response scope OVERRIDES the admin
	// streaming-mode knob (a UX/perf preference) — the live path is audit-only and
	// can never enforce, so an enforcing hook must never land on it. A hard-block
	// scope must BUFFER (zero-leak by contract, never best-effort streamed). A
	// redact scope under chunked_async ARMS Model A: real-time streaming with a
	// bounded tail + prescan gate that escalates to the canonical buffer on a
	// confirmed hit (runModelAStream); redact streaming is best-effort-wire by
	// design (see the streaming-compliance disclosure). A redact scope under
	// passthrough (raw forwarding — the boot default) cannot be honored on the
	// wire, so it falls back to BUFFER rather than streaming raw PII. An explicit
	// admin buffer_full_block already redacts in the buffer path for both scopes.
	if s.responseEnforcingBlock {
		streamMode = streampolicy.ModeBufferFullBlock
	} else if s.responseEnforcingRedact {
		if streamMode == streampolicy.ModeChunkedAsync {
			s.modelAArmed = true
		} else if streamMode != streampolicy.ModeBufferFullBlock {
			streamMode = streampolicy.ModeBufferFullBlock
		}
	}

	// Build a cross-format stream transcoder when the ingress and target wire
	// shapes differ. The transcoder converts canonical provider.Chunk fields
	// into ingress-native SSE frames so the client always receives the format
	// it expects. Returns nil for same-format pairs (passthrough).
	//
	// Cross-ingress override: when the cache HIT entry was written
	// under a different ingress wire shape (StreamHitOriginFromContext
	// returns ok=true with a non-matching BodyFormat), pick the
	// transcoder as if the "target" were the entry's origin wire shape.
	// That forces the chunkSSEReader to re-encode the cached canonical
	// chunks into the current ingress's SSE frames instead of forwarding
	// the cached RawBytes (which carry the writer's wire shape) verbatim.
	var transcoder canonicalbridge.StreamTranscoder
	var ingressFormat provcore.Format
	if ingress, ok := IngressFromContext(r.Context()); ok {
		ingressFormat = ingress.BodyFormat
		if h.deps.CanonicalBridge != nil {
			targetFormat := provcore.Format(target.AdapterType)
			origin, originOK := StreamHitOriginFromContext(r.Context())
			var originBodyFormat provcore.Format
			if originOK {
				var mapped bool
				originBodyFormat, mapped = WireShapeToBodyFormat(origin.WireShape)
				if !mapped {
					// Origin wire shape has no Format mapping (e.g. a future
					// Gemini/Vertex cache lane); skip the cross-ingress
					// transcoder override and let NewStreamTranscoder pick
					// the default for the current ingress + target pair.
					originOK = false
				} else {
					targetFormat = originBodyFormat
				}
			}
			transcoder = h.deps.CanonicalBridge.NewStreamTranscoder(ingress.BodyFormat, targetFormat, target.ModelCode)
			// Override edge case: the standard NewStreamTranscoder returns
			// nil for "ingress=FormatOpenAIResponses && target natively
			// serves Responses" (passthrough). On a cross-ingress cache
			// HIT where the cached chunks were written by a chat-completions
			// ingress, that passthrough would forward chat.completion SSE
			// frames to a /v1/responses client. Force the explicit ingress
			// encoder so the cached canonical chunks are re-encoded into
			// the request's wire SSE grammar.
			if originOK && transcoder == nil && originBodyFormat != ingress.BodyFormat {
				switch ingress.BodyFormat {
				case provcore.FormatOpenAIResponses:
					transcoder = canonicalbridge.NewResponsesStreamEncoder(target.ModelCode)
				default:
					if ingress.BodyFormat.IsOpenAIFamily() {
						transcoder = canonicalbridge.NewChatCompletionsStreamEncoder(target.ModelCode)
					}
				}
			}
		}
	}
	// Auto-upgrade: the client sent /v1/chat/completions but the upstream
	// actually got /v1/responses (its SSE is Responses-grammar chunks).
	// The (ingress=OpenAI, target=OpenAI) pair above resolved to nil
	// (same-format passthrough) — but we need to RE-ENCODE the chunks
	// back to chat-completions SSE so the chat-completions SDK can parse
	// them. Override with the chat-completions encoder.
	if ResponsesUpgradeFromContext(r.Context()) {
		transcoder = canonicalbridge.NewChatCompletionsStreamEncoder(target.ModelCode)
	}

	s.emitDone = emitDone
	s.streamMode = streamMode
	s.streamMaxBufferBytes = streamMaxBufferBytes
	s.transcoder = transcoder
	s.ingressFormat = ingressFormat
	return true
}
