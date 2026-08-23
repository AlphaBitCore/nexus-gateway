// stage_cache_body.go — prepareUpstreamBody: the cache stage's
// per-target upstream-body preparation (provider cache-control injection
// + cache-key strip). Split from stage_cache.go under the file-size
// ratchet; same package, same cacheStage receiver.
package proxy

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
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
	if len(s.routeResult.AllTargets()) == 0 {
		return true, false
	}
	primary := s.routeResult.Primary()
	adapter, ok := h.deps.ProviderReg.Get(provcore.Format(primary.AdapterType))
	if !ok {
		return true, false
	}

	// PrepareBody runs the model-alias rewrite + codec translation the
	// executor would otherwise do internally, so the first attempt of a
	// cache MISS reuses these bytes. bodyPrepCallTarget selects exactly the
	// fields that shape the body; its contract explains why the wire fields
	// are omitted and why the bytes must match the executor's re-prepared
	// target on retry/failover.
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
	prepReq.Target = bodyPrepCallTarget(primary)
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
	isImagesIngress := ingressKind == typology.EndpointKindImageGeneration
	isRerankIngress := ingressKind == typology.EndpointKindRerank
	needsCanonicalization := false
	if h.deps.CanonicalBridge != nil {
		switch {
		case s.resolved.WireShape == typology.WireShapeOpenAIResponses:
			// Responses is chat-kind but has its own native-passthrough
			// rule (only targets that natively serve /v1/responses).
			needsCanonicalization = !h.deps.CanonicalBridge.ServesResponses(targetFmt, primary.ServesResponsesAPI, s.body)
		case ingressKind == typology.EndpointKindChat, isEmbeddingsIngress, isImagesIngress, isRerankIngress:
			// Images: this stage always runs (the modality cache-skip lane
			// still prepares the upstream body), so without this arm a
			// Gemini-routed image request would hand WireShapeOpenAIImages
			// to the Gemini codec and die with a prepare-body 400 — the
			// proxy prepare decision, the executor arm, and the egress skip
			// must all agree (dispatch site 1 of 3).
			needsCanonicalization = s.resolved.BodyFormat != targetFmt
		}
	}
	// The rerank canonical IS the Cohere ingress shape, so a native Cohere
	// target needs no canonicalization — and the block below, which is the
	// only place the rerank contract is checked, is therefore skipped on the
	// commonest leg. That left the documents ceiling inert exactly where it
	// matters: it is a billing guard (rerank bills one search unit per 100
	// documents), so an unbounded array multiplies upstream spend from one
	// request. Validate here instead, taking only the verdict — the returned
	// bytes are the input unchanged, and assigning them would drag the body,
	// BodyFormat and WireShape rewrites below onto a leg that must stay a
	// verbatim passthrough.
	if isRerankIngress && !needsCanonicalization && h.deps.CanonicalBridge != nil {
		if err := h.deps.CanonicalBridge.ValidateRerankIngressGuards(
			s.resolved.BodyFormat, prepReq.Body, prepReq.Target); err != nil {
			h.writeCodecErr(s.w, s.rec, err, "canonicalize ingress body: ")
			return false, false
		}
	}
	// Same gap, same shape, on the image lane: `n` multiplies per-image spend
	// from one request, and the ceiling was enforced only where a body had to
	// be translated. A native OpenAI image request went to the upstream ungated.
	if isImagesIngress && !needsCanonicalization && h.deps.CanonicalBridge != nil {
		if err := h.deps.CanonicalBridge.ValidateImagesIngressGuards(
			s.resolved.BodyFormat, prepReq.Body, prepReq.Target); err != nil {
			h.writeCodecErr(s.w, s.rec, err, "canonicalize ingress body: ")
			return false, false
		}
	}
	if needsCanonicalization {
		var canonBody []byte
		var canonErr error
		switch {
		case isEmbeddingsIngress:
			canonBody, canonErr = h.deps.CanonicalBridge.IngressEmbeddingsToCanonical(s.resolved.BodyFormat, prepReq.Body, prepReq.Target)
		case isImagesIngress:
			// Validation + identity (the image canonical IS the OpenAI
			// ingress shape): prompt shape, the n fan-out bound, and the
			// nexus-key reject bind here for the primary target.
			canonBody, canonErr = h.deps.CanonicalBridge.IngressImagesToCanonical(s.resolved.BodyFormat, prepReq.Body, prepReq.Target)
		case isRerankIngress:
			// Validation + identity (the rerank canonical IS the Cohere
			// ingress shape): query/documents/top_n validation binds here
			// for the primary target.
			canonBody, canonErr = h.deps.CanonicalBridge.IngressRerankToCanonical(s.resolved.BodyFormat, prepReq.Body, prepReq.Target)
		default:
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
			h.writeCodecErr(s.w, s.rec, canonErr, "canonicalize ingress body: ")
			return false, false
		}
		prepReq.Body = canonBody
		// Rerank's canonical format is Cohere (not OpenAI) — the ingress
		// body IS the Cohere-shaped canonical; every other kind
		// canonicalizes to the OpenAI shape.
		if isRerankIngress {
			prepReq.BodyFormat = provcore.FormatCohere
		} else {
			prepReq.BodyFormat = provcore.FormatOpenAI
		}
		// The cache-prep codec must encode to the TARGET adapter's
		// native wire shape (e.g. anthropic-messages, gemini embedContent),
		// not the caller's ingress shape — otherwise the target codec
		// rejects "openai-chat"/"openai-embeddings". This matches the bytes
		// the executor produces (cache-key + MISS-reuse parity).
		switch {
		case isEmbeddingsIngress:
			prepReq.WireShape = h.deps.CanonicalBridge.EmbeddingsWireShapeForTarget(targetFmt)
		case isImagesIngress:
			prepReq.WireShape = h.deps.CanonicalBridge.ImagesWireShapeForTarget(targetFmt)
		case isRerankIngress:
			prepReq.WireShape = h.deps.CanonicalBridge.RerankWireShapeForTarget(targetFmt)
		default:
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
	// Attempt-0 sends cachePreparedBody and never reaches IngressChatToWire.
	// Apply the same consumer boundary after either canonicalization or the
	// native identity path, so identity OpenAI requests cannot leak either
	// provider-private carrier.
	if h.deps.CanonicalBridge != nil && ingressKind == typology.EndpointKindChat {
		prepReq.Body = h.deps.CanonicalBridge.StripInternalCarriersForTarget(prepReq.Body, targetFmt)
	}
	prepStart := time.Now()
	finalBody, finalRewrites, finalURLOverride, err := adapter.PrepareBody(prepReq)
	if err != nil {
		// Routing happened, so record which model it chose even though the codec
		// refused before a byte left the gateway. Measured — an upstream refusal
		// recorded gpt-4o-mini/openai while our own recorded null/null, leaving
		// "how often does our codec refuse what auto picked" unanswerable.
		stampRoutedTarget(s.rec, primary)
		h.writeCodecErr(s.w, s.rec, err, "prepare body: ")
		return false, false
	}
	s.phaseTimer.MarkBetween(traffic.PhaseReqAdapter, time.Since(prepStart))
	s.cachePreparedBody = finalBody
	s.cachePreparedRewrites = finalRewrites
	s.cachePreparedURLOverride = finalURLOverride
	return true, true
}

// bodyPrepCallTarget projects a routing-snapshot target onto the CallTarget
// subset that shapes the upstream body — the single definition of which fields
// PrepareBody is allowed to read. The executor's own CallTarget
// (provtarget.PgResolver.Resolve) additionally carries wire/dispatch fields
// (APIKey, credential identity, Extras); those must not influence the body,
// or the cache stage's first-attempt body would diverge from the executor's
// re-prepared retry/failover body — breaking both the cache key and
// PrepareBody idempotency. TestBodyPrepCallTarget_WireFieldsInert pins that
// contract.
//
// ServesResponsesAPI is deliberately IN the projection: the dispatch-level
// native-leg triage reads it (a /v1/responses body headed to a
// responses-serving target takes the RewriteNative differential), so it is a
// body-shaping field — omitting it here would make the cache-prep and
// executor legs triage the same request differently.
func bodyPrepCallTarget(t routingcore.RoutingTarget) provcore.CallTarget {
	return provcore.CallTarget{
		ProviderID:         t.ProviderID,
		ProviderName:       t.ProviderName,
		Format:             provcore.Format(t.AdapterType),
		ProviderModelID:    t.ProviderModelID,
		BaseURL:            t.BaseURL,
		MaxOutputTokens:    t.MaxOutputTokens,
		Reasons:            t.Reasons,
		ServesResponsesAPI: t.ServesResponsesAPI,
	}
}

// stampRoutedTarget serves every path ending a request after routing resolved
// a target but before the upstream was reached. The upstream paths stamp the
// same four fields from the executor's attributed attempt.
func stampRoutedTarget(rec *audit.Record, t routingcore.RoutingTarget) {
	if rec == nil || t.ModelID == "" {
		return
	}
	rec.RoutedProviderID = t.ProviderID
	rec.RoutedProviderName = t.ProviderName
	rec.RoutedModelID = t.ModelID
	rec.RoutedModelName = t.ModelCode
}
