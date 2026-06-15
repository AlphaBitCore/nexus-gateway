// stage_cache_body.go — prepareUpstreamBody: the cache stage's
// per-target upstream-body preparation (provider cache-control injection
// + cache-key strip). Split from stage_cache.go under the file-size
// ratchet; same package, same cacheStage receiver.
package proxy

import (
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// prepareUpstreamBody resolves the primary adapter and runs the alias-rewrite +
// codec translation (PrepareBody, idempotent with the executor's own run),
// setting s.cachePreparedBody/Rewrites/URLOverride. It exists APART from the
// cache lookup because the prepared body also feeds the upstream-body
// normaliser (provider cache_control / cachedContent injection) — a request
// the SEMANTIC cache skips (time-sensitive, disabled, client no-cache) must still get
// its provider-side cache markers, or skipping one cache silently disables
// the other (live incident: ~0% Anthropic prompt-cache on the assistant's
// own traffic). ok=false → an error response was already written; prepared
// reports whether the body was actually prepared (false on the defensive
// adapter-missing path).
func (st cacheStage) prepareUpstreamBody() (ok bool, prepared bool) {
	s := st.s
	h := s.h
	// The routing stage guarantees ≥1 target before the cache stage runs; if
	// that invariant ever regresses, degrade to unprepared (the executor
	// fails the request with its own named error) instead of panicking here.
	if len(s.routeResult.Targets) == 0 {
		return true, false
	}
	primary := s.routeResult.Targets[0]
	adapter, ok := h.deps.ProviderReg.Get(provcore.Format(primary.AdapterType))
	if !ok {
		return true, false
	}

	// PrepareBody runs the model-alias rewrite + codec
	// translation that the executor would otherwise do
	// internally. Only ProviderModelID and Format on the
	// CallTarget matter for body preparation; the executor
	// resolves the full target (BaseURL, APIKey, Extras)
	// on the wire path. PrepareBody is idempotent so the
	// executor running it again on the MISS path produces
	// the same bytes.
	//
	// G3 (provider-adapter-architecture.md §11): PrepareBody's
	// codec contract requires canonical OpenAI input. When the
	// caller's ingress format differs from the target format,
	// canonicalize via the bridge first. Without this step a
	// cross-format route (e.g. Anthropic ingress → OpenAI
	// target) would hand the Anthropic-shape body to
	// openairesponses.identityCodec (identity), which forwards it
	// verbatim and the upstream 400s.
	prepReq := buildProviderRequest(s.r, s.resolved, s.body, s.isStream, h.payloadCaptureConfig().MaxResponseBytes)
	prepReq.Target = provcore.CallTarget{
		ProviderID:      primary.ProviderID,
		ProviderName:    primary.ProviderName,
		Format:          provcore.Format(primary.AdapterType),
		ProviderModelID: primary.ProviderModelID,
		BaseURL:         primary.BaseURL,
	}
	// Cross-format canonicalization: "cross-format" depends on
	// the endpoint shape, not just the wire format string:
	//   - chat-completions ingress → canonicalize iff target wire
	//     format is not OpenAI (canonical = OpenAI chat-completions).
	//   - /v1/responses ingress    → canonicalize iff target wire
	//     format does NOT natively serve the Responses API.
	//     A naive `BodyFormat != AdapterType` check would
	//     misfire here because FormatOpenAIResponses !=
	//     FormatOpenAI even when the target IS OpenAI — that
	//     turned a native passthrough into a canonicalize, and
	//     OpenAI returned 400 "Unsupported parameter: 'messages'.
	//     In the Responses API…".
	//
	// When we canonicalize a /v1/responses request, both
	// prepReq.WireShape AND resolved.WireShape must be downgraded
	// to WireShapeOpenAIChat. prepReq.WireShape drives the
	// codec (spec_anthropic / spec_gemini only know
	// "chat-completions" — without the downgrade they return
	// `<provider>: unsupported endpoint "responses" for codec`).
	// resolved.WireShape is what fetchUpstreamWithPreparedBody later
	// hands to buildProviderRequest, which drives the URL
	// builder — without the downgrade the URL builder returns
	// `build url: <provider>: unsupported endpoint "responses"`.
	// The egress reshape path keys off resolved.BodyFormat (still
	// FormatOpenAIResponses), so the client still sees a
	// Responses-shape body.
	// Per-endpoint canonicalization decision:
	//   chat-completions: canonicalize whenever ingress ≠ target
	//     wire format. The downstream codec dispatch in
	//     specAdapter.PrepareBody handles OpenAI-wire-shape
	//     passthrough (Moonshot/Mistral/Groq/...) by matching on
	//     IsOpenAIFamily() AFTER canonicalization. So
	//     Anthropic→OpenAI / Gemini→Mistral / etc. all flow
	//     through the bridge; OpenAI→OpenAI doesn't because
	//     formats already match.
	//   /v1/responses: canonicalize only when the target adapter
	//     does NOT natively serve responses-api. The naive
	//     `BodyFormat != AdapterType` check misfires here because
	//     FormatOpenAIResponses != FormatOpenAI even when the
	//     target IS OpenAI — that turned native passthrough
	//     into canonicalize and broke the Responses-shape body.
	// Cross-format canonicalization is driven by the ingress
	// EndpointKind, not a hardcoded openai-chat/responses list, so
	// EVERY chat-kind ingress (openai-chat, anthropic /v1/messages,
	// gemini generateContent, Azure, GLM) gets the same canonical →
	// target-wire translation. "ingress shape in = ingress shape out"
	// is preserved end-to-end: resolved.WireShape (the caller's shape)
	// is left intact, and the executor derives the call-time wire
	// shape from the target while egress reshapes via the immutable
	// context ingress.
	targetFmt := provcore.Format(primary.AdapterType)
	ingressKind := typology.KindFromWireShape(s.resolved.WireShape)
	isEmbeddingsIngress := ingressKind == typology.EndpointKindEmbeddings
	needsCanonicalization := false
	if h.deps.CanonicalBridge != nil {
		switch {
		case s.resolved.WireShape == typology.WireShapeOpenAIResponses:
			// Responses is chat-kind but has its own native-passthrough
			// rule (only targets that natively serve /v1/responses).
			needsCanonicalization = !h.deps.CanonicalBridge.TargetNativelyServesResponsesAPI(targetFmt)
		case ingressKind == typology.EndpointKindChat, isEmbeddingsIngress:
			needsCanonicalization = s.resolved.BodyFormat != targetFmt
		}
	}
	if needsCanonicalization {
		var canonBody []byte
		var canonErr error
		if isEmbeddingsIngress {
			canonBody, canonErr = h.deps.CanonicalBridge.IngressEmbeddingsToCanonical(s.resolved.BodyFormat, prepReq.Body, prepReq.Target)
		} else {
			canonBody, canonErr = h.deps.CanonicalBridge.IngressChatToCanonical(s.resolved.BodyFormat, prepReq.Body, prepReq.Target)
			// Stamp the streaming intent onto the canonical body. Gemini
			// ingress signals streaming via the :streamGenerateContent URL,
			// not a body field, so the canonical chat body carries no
			// `stream` — without this the target codec (e.g. Anthropic, which
			// propagates `stream` from canonical input) sends a non-streaming
			// upstream request and the client's SSE loses all text. Chat-kind
			// only; embeddings never stream.
			if canonErr == nil && s.isStream {
				canonBody = canonicalbridge.EnsureCanonicalStream(canonBody)
			}
		}
		if canonErr != nil {
			h.writeError(s.w, s.rec, http.StatusBadRequest, "canonicalize ingress body: "+canonErr.Error())
			return false, false
		}
		prepReq.Body = canonBody
		prepReq.BodyFormat = provcore.FormatOpenAI
		// The cache-prep codec must encode to the TARGET adapter's
		// native wire shape (e.g. anthropic-messages, gemini embedContent),
		// not the caller's ingress shape — otherwise the target codec
		// rejects "openai-chat"/"openai-embeddings". This matches the bytes
		// the executor produces (cache-key + MISS-reuse parity).
		if isEmbeddingsIngress {
			prepReq.WireShape = h.deps.CanonicalBridge.EmbeddingsWireShapeForTarget(targetFmt)
		} else {
			prepReq.WireShape = h.deps.CanonicalBridge.ChatWireShapeForTarget(targetFmt)
		}
		if s.resolved.WireShape == typology.WireShapeOpenAIResponses {
			// /v1/responses canonicalizes to chat-completions. Downgrade
			// the per-request resolved copy (not s.in, the shared
			// route-table descriptor) so the executor treats it as
			// chat-kind on the failover path. resolved.BodyFormat stays
			// FormatOpenAIResponses so egress still hits the Responses
			// encoder (egress reads the immutable context ingress).
			s.resolved.WireShape = typology.WireShapeOpenAIChat
		}
	}
	prepStart := time.Now()
	finalBody, finalRewrites, finalURLOverride, err := adapter.PrepareBody(prepReq)
	if err != nil {
		h.writeError(s.w, s.rec, http.StatusBadRequest, "prepare body: "+err.Error())
		return false, false
	}
	s.phaseTimer.MarkBetween(traffic.PhaseReqAdapter, time.Since(prepStart))
	s.cachePreparedBody = finalBody
	s.cachePreparedRewrites = finalRewrites
	s.cachePreparedURLOverride = finalURLOverride
	return true, true
}
