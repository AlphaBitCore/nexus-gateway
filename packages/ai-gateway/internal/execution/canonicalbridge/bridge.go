// Package canonicalbridge translates between client ingress wire formats and
// upstream provider wire formats using OpenAI chat.completions JSON as the
// internal hub representation for the chat endpoint.
package canonicalbridge

import (
	"fmt"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/cohere"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/sjson"
)

// formatsNativelyServingResponsesAPI is the set of provider wire formats whose
// adapter declares AdapterSpec.RequestShapes ⊇ "responses-api". The bridge
// consults this set on the /v1/responses ingress path — when the target's
// Format is in this set, the body is forwarded verbatim and no
// Responses↔canonical codec runs. Kept in lockstep with each adapter's
// spec.go RequestShapes declaration (canonical truth:
// providers/spec_openai/spec.go).
//
// Adding a sibling adapter: per provider-adapter-architecture.md §3a Rule 7
// (binding), confirm via a captured 200 from that provider's real /v1/responses
// endpoint before adding to this set AND the adapter's RequestShapes.
var formatsNativelyServingResponsesAPI = map[provcore.Format]bool{
	provcore.FormatOpenAI: true,
}

// Bridge performs ingress ↔ canonical ↔ target wire conversions for chat.
type Bridge struct {
	codecs map[provcore.Format]provcore.SchemaCodec
}

// New builds a bridge backed by per-format SchemaCodec instances (typically
// from [github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins.SchemaCodecs]).
func New(codecs map[provcore.Format]provcore.SchemaCodec) *Bridge {
	return &Bridge{codecs: codecs}
}

// openAILike reports whether a provider format speaks the OpenAI Chat
// Completions wire schema natively (request body, response body, SSE
// frame envelope). Delegates to provcore.Format.IsOpenAIFamily —
// the same canonical list that spec_openai's IdentityCodec covers —
// so the canonical bridge never under-counts OpenAI-shape providers
// (Moonshot, Mistral, Groq, Together, Fireworks, Perplexity, XAI,
// MiniMax, HuggingFace) and rejects them as if they needed a
// translator. Pre-fix, an OpenAI-ingress streaming request that
// auto-routed to e.g. Moonshot was rejected at the cross-format
// stream gate (writeCrossFormatStreamUnsupported) even though both
// sides speak the same wire shape.
func openAILike(f provcore.Format) bool {
	return f.IsOpenAIFamily()
}

func ingressSupportsHubCanonicalChat(ingress provcore.Format) bool {
	switch ingress {
	case provcore.FormatAnthropic, provcore.FormatGemini, provcore.FormatVertex:
		return true
	default:
		return false
	}
}

// formatSupportsChat reports whether a provider format has a chat-completions
// codec at all. Embeddings-only providers (Voyage today; future audio/image-
// only providers) return false so ChatRoutable does not consider them valid
// chat targets even though their format value is otherwise registered.
func formatSupportsChat(f provcore.Format) bool {
	if !f.Valid() {
		return false
	}
	switch f {
	case provcore.FormatVoyage:
		// Voyage is embeddings-only (RequestShapes: ["embeddings"]).
		return false
	default:
		return true
	}
}

// ChatRoutable reports whether chat completions traffic from ingressFormat may
// be routed to a target that uses targetFormat.
func (b *Bridge) ChatRoutable(ingress, target provcore.Format) bool {
	if ingress == target {
		return formatSupportsChat(ingress)
	}
	if !formatSupportsChat(target) {
		return false
	}
	if openAILike(ingress) && formatSupportsChat(target) {
		return true
	}
	if openAILike(target) && ingressSupportsHubCanonicalChat(ingress) {
		return true
	}
	return ingressSupportsHubCanonicalChat(ingress) && formatSupportsChat(target)
}

// TargetNativelyServesResponsesAPI reports whether a target provider wire format
// natively serves /v1/responses (adapter declares RequestShapes ⊇ "responses-api").
func (b *Bridge) TargetNativelyServesResponsesAPI(target provcore.Format) bool {
	return formatsNativelyServingResponsesAPI[target]
}

// ResponsesRoutable reports whether /v1/responses ingress traffic
// (FormatOpenAIResponses, EndpointResponsesAPI) may be routed to a
// target that uses targetFormat. Mirrors ChatRoutable for the new
// endpoint:
//   - Native same-shape passthrough: target adapter declares
//     responses-api support (checked via the lockstep map above).
//   - Cross-format canonical bridge: any otherwise-valid chat target
//     (target.Valid()), reached via DecodeResponsesRequest → canonical
//     chat-completions → target's codec.EncodeRequest.
//
// EndpointResponsesAPI traffic is rejected against Bedrock+ since
// Bedrock uses AWS binary event-stream framing on streams (no SSE,
// no chat-completions wire shape on responses).
func (b *Bridge) ResponsesRoutable(target provcore.Format) bool {
	if formatsNativelyServingResponsesAPI[target] {
		return true
	}
	// Cross-format path requires the target to have a chat-completions
	// codec we can encode canonical into.
	if !target.Valid() {
		return false
	}
	// Bedrock streaming uses AWS event-stream framing, not SSE — its
	// response codec doesn't fit the Responses event grammar. Reject
	// cross-format routing to Bedrock for Responses ingress (mirrors
	// the StreamShapeCompatible exclusion).
	if target == provcore.FormatBedrock {
		return false
	}
	return true
}

// EmbeddingsRoutable reports whether /v1/embeddings traffic from ingress may be
// routed to a target. Routable pairs:
//   - same-format passthrough (any registered ingress/target pair)
//   - OpenAI / Azure (OpenAI-wire-shape) ingress → any registered target
//     whose codec implements EndpointEmbeddings
//   - Cohere / Gemini ingress → OpenAI / Azure / Cohere / Gemini target
//
// The capability pre-filter is responsible for dimension / batch-size /
// encoding-format asymmetry checks. EmbeddingsRoutable only gates
// wire-shape translatability.
func (b *Bridge) EmbeddingsRoutable(ingress, target provcore.Format) bool {
	if ingress == target {
		return true
	}
	if !ingress.Valid() || !target.Valid() {
		return false
	}
	// Codec registration is a necessary precondition for any
	// canonical-bridge-mediated routing — the codec is the wire-shape
	// translator on the target side. The capability bridge (b.codecs)
	// is populated by builtins.SchemaCodecs at startup.
	if _, ok := b.codecs[ingress]; !ok {
		return false
	}
	if _, ok := b.codecs[target]; !ok {
		return false
	}
	// In-scope ingress formats: OpenAI / Azure (OpenAI-wire-shape), Cohere, Gemini.
	// Other formats may have codecs registered for chat but no embedding canonical
	// helper yet — reject until extended.
	switch {
	case ingress.IsOpenAIFamily():
		// OK
	case ingress == provcore.FormatCohere:
		// OK
	case ingress == provcore.FormatGemini, ingress == provcore.FormatVertex:
		// OK
	default:
		return false
	}
	switch {
	case target.IsOpenAIFamily():
		return true
	case target == provcore.FormatCohere:
		return true
	case target == provcore.FormatGemini, target == provcore.FormatVertex:
		return true
	default:
		return false
	}
}

// EndpointRoutable reports whether an ingress format may route to a target
// format for the given endpoint wire shape.
//
// It dispatches by endpoint KIND (typology.KindFromWireShape) so the routing
// gate stays in lockstep with the rest of the pipeline: request
// canonicalization (proxy.go IngressChatToCanonical) and egress reshape
// (ResponseCanonicalToIngress) both classify the endpoint via
// KindFromWireShape. A hardcoded per-WireShape switch previously routed only
// WireShapeOpenAIChat through ChatRoutable, so every OTHER chat-kind ingress
// (anthropic-messages, gemini / vertex generate-content) fell into a
// same-format-only default — silently blocking the cross-provider routing
// those ingresses are otherwise fully built to serve. Dispatching by Kind
// closes that drift: any chat-kind WireShape gets ChatRoutable.
//
// WireShapeOpenAIResponses and the legacy/none shapes keep dedicated rules and
// are matched BEFORE the Kind dispatch (both are chat-kind, but Responses has
// its own native-passthrough gate and /v1/completions + model-listing have no
// canonical translation pipeline for their payload shape).
func (b *Bridge) EndpointRoutable(ep typology.WireShape, ingress, target provcore.Format) bool {
	switch ep {
	case typology.WireShapeOpenAIResponses:
		// /v1/responses ingress: cross-format goes through canonical chat-completions;
		// same-shape passthrough fires for targets declaring responses-api support.
		// Strictly an OpenAI-Responses ingress.
		return ingress == provcore.FormatOpenAIResponses && b.ResponsesRoutable(target)
	case typology.WireShapeNone, typology.WireShapeOpenAICompletionsLegacy:
		// Model listing + legacy /v1/completions have no canonical translation
		// pipeline for their payload shape: same-format, or the legacy
		// OpenAI-only translation rule.
		if ingress == target {
			return true
		}
		return ingress == provcore.FormatOpenAI
	}
	switch typology.KindFromWireShape(ep) {
	case typology.EndpointKindChat:
		return b.ChatRoutable(ingress, target)
	case typology.EndpointKindEmbeddings:
		// Cross-format routing via the embedding canonical bridge.
		// Capability mismatch (dimensions / batch / encoding) surfaces at the
		// routing pre-filter, not here.
		return b.EmbeddingsRoutable(ingress, target)
	default:
		return ingress == target
	}
}

// StreamShapeCompatible reports whether streaming between ingress and target is
// supported. Every valid non-Bedrock pair is compatible: each ingress format has
// a registered StreamTranscoder that re-encodes canonical chunks into the
// ingress-native SSE frames. Bedrock is excluded because it uses AWS binary
// event-stream framing rather than HTTP SSE.
func StreamShapeCompatible(ingress, target provcore.Format) bool {
	if !ingress.Valid() || !target.Valid() {
		return false
	}
	if ingress == provcore.FormatBedrock || target == provcore.FormatBedrock {
		return false
	}
	return true
}

// NewStreamTranscoder returns a StreamTranscoder for the given ingress→target
// pair, or nil when the pair is a passthrough (same format or both OpenAI-like)
// that does not require re-encoding. Callers attach the returned transcoder to
// the chunkSSEReader so canonical provider chunks are re-encoded into the
// ingress-native SSE frame format before being forwarded to the client.
//
// model is the customer-facing model code (e.g. "claude-opus-4-7") used to
// populate the `model` field in synthesised OpenAI SSE chunks when the ingress
// is OpenAI-like but the upstream speaks a different wire format.
func (b *Bridge) NewStreamTranscoder(ingress, target provcore.Format, model string) StreamTranscoder {
	// Passthrough: raw provider SSE bytes can be forwarded directly.
	if ingress == target {
		return nil
	}
	if openAILike(ingress) && openAILike(target) {
		return nil
	}
	// /v1/responses ingress + native responses-api target (today only
	// spec_openai): the upstream's Responses SSE bytes flow through
	// unchanged. No transcoder needed.
	if ingress == provcore.FormatOpenAIResponses && formatsNativelyServingResponsesAPI[target] {
		return nil
	}
	switch {
	case ingress == provcore.FormatOpenAIResponses:
		// Cross-format: target is chat-completions wire. The reverse encoder
		// re-shapes canonical chunks into Responses SSE event grammar before
		// forwarding to the /v1/responses client.
		return newResponsesStreamEncoder(model)
	case openAILike(ingress):
		return newOpenAIStreamEncoder(model)
	case ingress == provcore.FormatAnthropic:
		return newAnthropicStreamEncoder()
	case ingress == provcore.FormatGemini, ingress == provcore.FormatVertex:
		return &geminiStreamEncoder{}
	case ingress == provcore.FormatCohere:
		return &cohereStreamEncoder{}
	case ingress == provcore.FormatReplicate:
		return &replicateStreamEncoder{}
	default:
		// Unknown ingress: fall back to passthrough and let the executor
		// surface any wire-format mismatch as an upstream error.
		return nil
	}
}

// DecodeViaShared is the single entry-point for response-side decoding
// (wire native → canonical OpenAI chat-completion JSON + extracted Usage).
// Each provider's SchemaCodec.DecodeResponse is the authoritative projection,
// but routing every response through this helper guarantees that all callers
// (proxy handler, /v1/estimate, audit Recorder) see byte-identical Usage values
// and that any future Usage normalization rule lands in one place (shared/normalize).
//
// Returns (canonicalBody, Usage, error). On a nil body it returns zero values
// with no error (the audit pipeline treats Usage{} as "not reported").
// Architectural reference: cost-estimation-architecture.md §1.
func (b *Bridge) DecodeViaShared(format provcore.Format, endpoint typology.WireShape, body []byte) ([]byte, provcore.Usage, error) {
	codec, ok := b.codecs[format]
	if !ok || codec == nil {
		return body, provcore.ExtractUsage(body, format), nil
	}
	// Post-hoc shared decode (cache HIT replay reshape / estimate compare /
	// audit Recorder) re-decodes an already-captured response body without
	// the originating request, so the request-relative codec checks (embedding
	// count guard, usage estimate) are not applicable here — pass a zero
	// DecodeContext to fail them open.
	decodeRes, err := codec.DecodeResponse(endpoint, body, "", provcore.DecodeContext{})
	if err != nil {
		return body, provcore.Usage{}, err
	}
	canonical := decodeRes.CanonicalBody
	// Usage is sourced from the shared/normalize Tier-1 normalizer via
	// provcore.ExtractUsage so the cache HIT replay path + the
	// estimate compare endpoint + the audit Recorder all see the same
	// canonical numbers. We discard the codec's local Usage return
	// value here on purpose: keeping a single Usage source kills any
	// possibility of drift between the two extraction paths.
	usage := provcore.ExtractUsage(body, format)
	return canonical, usage, nil
}

// IngressChatToCanonical converts the client ingress JSON to canonical OpenAI
// chat.completions request JSON.
func (b *Bridge) IngressChatToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error) {
	if openAILike(ingress) {
		return body, nil
	}
	switch ingress {
	case provcore.FormatAnthropic:
		return anthropic.MessagesRequestToOpenAIChatCompletion(body, ct.ProviderModelID)
	case provcore.FormatGemini, provcore.FormatVertex:
		// Vertex egress uses the Gemini SchemaCodec; native ingress bodies match
		// the same generateContent JSON shape.
		return gemini.GenerateContentRequestToOpenAIChatCompletion(body, ct.ProviderModelID)
	case provcore.FormatOpenAIResponses:
		// /v1/responses request body → canonical chat-completions.
		// Stateful fields (previous_response_id, store, truncation, built-in
		// tools) ride along under nexus.ext.openai.responses.* for the
		// cross-format guard to inspect.
		return openai.DecodeResponsesRequest(body)
	default:
		return nil, fmt.Errorf("canonicalbridge: ingress format %q has no chat hub codec", ingress)
	}
}

// IngressChatToWire converts ingress JSON to the upstream provider wire body
// for chat completions. `stream` is the request's resolved streaming intent
// (req.Stream): ingresses that signal streaming out-of-band — Gemini's
// :streamGenerateContent URL — produce a canonical body with no `stream` field,
// so it must be stamped on before the target codec encodes, or a codec that
// propagates `stream` from the canonical input (Anthropic) emits a
// non-streaming upstream request and the client's stream loses all content.
func (b *Bridge) IngressChatToWire(ingress, target provcore.Format, body []byte, ct provcore.CallTarget, stream bool) ([]byte, error) {
	if ingress == target {
		return body, nil
	}
	// /v1/responses ingress + a target whose adapter natively serves
	// responses-api (today: spec_openai): forward the body verbatim.
	// This is the capability-driven same-shape passthrough — adding a
	// new sibling to formatsNativelyServingResponsesAPI activates the
	// fast path for them without further code changes here.
	if ingress == provcore.FormatOpenAIResponses && formatsNativelyServingResponsesAPI[target] {
		return body, nil
	}
	canon, err := b.IngressChatToCanonical(ingress, body, ct)
	if err != nil {
		return nil, err
	}
	if stream {
		canon = EnsureCanonicalStream(canon)
	}
	codec, ok := b.codecs[target]
	if !ok || codec == nil {
		return nil, fmt.Errorf("canonicalbridge: no codec for format %q", target)
	}
	encRes, err := codec.EncodeRequest(chatWireShapeForFormat(target), canon, ct)
	return encRes.Body, err
}

// EnsureCanonicalStream stamps `stream: true` onto an OpenAI-canonical chat
// request body. Cross-format ingresses that signal streaming out-of-band — the
// Gemini :streamGenerateContent URL, the gateway's broker stream path —
// canonicalize to a chat body that carries no `stream` field. A target codec
// that propagates the field from its canonical input (the Anthropic codec reads
// `stream` and forwards it to /v1/messages) would otherwise emit a NON-streaming
// upstream request, so the client's SSE terminates with no text. Callers invoke
// this only when the request is actually streaming (gated on the resolved
// isStream / req.Stream), so the set is unconditional. Gemini/Vertex targets
// drive streaming from the URL and ignore the body field — the stamp is a
// harmless no-op for them.
func EnsureCanonicalStream(canonicalBody []byte) []byte {
	if len(canonicalBody) == 0 {
		return canonicalBody
	}
	out, err := sjson.SetBytes(canonicalBody, "stream", true)
	if err != nil {
		return canonicalBody
	}
	return out
}

// chatWireShapeForFormat returns the chat-completions wire shape for a target
// provider format. Used by the canonical bridge to dispatch codec encode/decode
// calls to the wire shape the target's codec actually serves (Anthropic codec
// expects WireShapeAnthropicMessages, Gemini codec expects
// WireShapeGeminiGenerateContent, etc.).
//
// This is NOT a compat shim. It is the structural projection (Format → native
// chat WireShape) that has to live somewhere — Format identifies the adapter
// family (b.codecs registry key, routing key, target provider) while WireShape
// identifies the per-request wire shape that codecs dispatch on. An adapter
// family has 1-3 native wire shapes; this lookup picks the chat one.
//
// Lockstep contract: every Format that maps to an adapter with a chat-shape
// codec MUST appear in this switch (or fall through to the OpenAI-family
// default). Adding a new chat adapter requires adding its Format → native
// chat WireShape mapping here.
// ChatWireShapeForTarget exposes chatWireShapeForFormat on the API so the
// ingress handler can resolve a target's native chat wire shape after
// canonicalizing a cross-format request (without leaking the caller's ingress
// shape into the target adapter's codec).
func (b *Bridge) ChatWireShapeForTarget(target provcore.Format) typology.WireShape {
	return chatWireShapeForFormat(target)
}

// EmbeddingsWireShapeForTarget is the embeddings-kind counterpart of
// ChatWireShapeForTarget.
func (b *Bridge) EmbeddingsWireShapeForTarget(target provcore.Format) typology.WireShape {
	return embeddingsWireShapeForFormat(target)
}

func chatWireShapeForFormat(f provcore.Format) typology.WireShape {
	switch f {
	case provcore.FormatAnthropic:
		return typology.WireShapeAnthropicMessages
	case provcore.FormatGemini:
		return typology.WireShapeGeminiGenerateContent
	case provcore.FormatVertex:
		return typology.WireShapeVertexGenerateContent
	case provcore.FormatBedrock:
		return typology.WireShapeBedrockConverse
	case provcore.FormatCohere:
		return typology.WireShapeCohereChat
	}
	// Default: every other Format is OpenAI-wire-shape (openai, deepseek,
	// glm, azure-openai, mistral, xai, groq, perplexity, together,
	// fireworks, moonshot, minimax, huggingface, replicate). Their codecs
	// all dispatch on WireShapeOpenAIChat.
	return typology.WireShapeOpenAIChat
}

// embeddingsWireShapeForFormat returns the embeddings wire shape for a target
// provider format. Mirrors chatWireShapeForFormat for the embeddings endpoint;
// see that helper's doc-comment for why this projection lives here (structural
// Format → native WireShape mapping, not a compat shim).
func embeddingsWireShapeForFormat(f provcore.Format) typology.WireShape {
	switch f {
	case provcore.FormatGemini:
		return typology.WireShapeGeminiEmbedContent
	case provcore.FormatVertex:
		return typology.WireShapeVertexEmbedContent
	case provcore.FormatBedrock:
		return typology.WireShapeBedrockEmbeddings
	case provcore.FormatCohere:
		return typology.WireShapeCohereEmbed
	case provcore.FormatVoyage:
		return typology.WireShapeVoyageEmbeddings
	}
	// Default: OpenAI-wire family.
	return typology.WireShapeOpenAIEmbeddings
}

// IngressEmbeddingsToCanonical converts a client ingress embedding
// request body into canonical OpenAI /v1/embeddings JSON.
//
//   - OpenAI / Azure (OpenAI-wire) ingress → identity (canonical IS OpenAI).
//   - Cohere ingress (POST /v1/embed or /v2/embed) → cohere helper.
//   - Gemini / Vertex ingress :embedContent (single) and :batchEmbedContents
//     (batch) → gemini helper. The bridge cannot distinguish single vs
//     batch from the body alone (a single-element `requests` array vs a
//     plain `content` object are both valid), so the caller must thread
//     the choice via `singleEndpoint` when known. When unknown, the
//     helper detects on body shape (presence of "requests": batch path;
//     otherwise single).
//
// The ingress bridge is a pure translation step — capability
// checks (dimensions / batch size) belong to the routing pre-filter and
// codec safety net.
func (b *Bridge) IngressEmbeddingsToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error) {
	if openAILike(ingress) {
		return body, nil
	}
	switch ingress {
	case provcore.FormatCohere:
		return cohere.EmbedRequestToCanonical(body, ct.ProviderModelID)
	case provcore.FormatGemini, provcore.FormatVertex:
		// Detect single vs batch by body shape: ":batchEmbedContents"
		// requests carry a top-level "requests" array; ":embedContent"
		// carries a top-level "content" object.
		if hasJSONField(body, "requests") {
			return gemini.BatchEmbedContentsRequestToCanonical(body, ct.ProviderModelID)
		}
		return gemini.EmbedContentRequestToCanonical(body, ct.ProviderModelID)
	default:
		return nil, fmt.Errorf("canonicalbridge: ingress format %q has no embeddings hub codec", ingress)
	}
}

// IngressEmbeddingsToWire mirrors [Bridge.IngressChatToWire] for embeddings.
// Same-format ingress=target → passthrough; cross-format goes through
// canonical → target codec.
//
// Unlike the chat counterpart, the embeddings codec can emit an
// EncodeResult.URLOverride to select between distinct upstream endpoints for
// the SAME wire shape — Gemini's :embedContent (single input) vs
// :batchEmbedContents (array input). That action suffix is encoded only by the
// codec (it inspects the canonical `input` cardinality), so it MUST be returned
// to the caller and threaded onto the dispatched URL; dropping it sends the
// batch body ({"requests":[…]}) to the single-embed URL, which Gemini rejects
// with `Unknown name "requests": Cannot find field`.
// urlOverride is "" on the same-format passthrough and for codecs that do not
// switch endpoints (OpenAI/Cohere embeddings).
func (b *Bridge) IngressEmbeddingsToWire(ingress, target provcore.Format, body []byte, ct provcore.CallTarget) (wireBody []byte, urlOverride string, err error) {
	if ingress == target {
		return body, "", nil
	}
	canon, err := b.IngressEmbeddingsToCanonical(ingress, body, ct)
	if err != nil {
		return nil, "", err
	}
	codec, ok := b.codecs[target]
	if !ok || codec == nil {
		return nil, "", fmt.Errorf("canonicalbridge: no codec for format %q", target)
	}
	encRes, err := codec.EncodeRequest(embeddingsWireShapeForFormat(target), canon, ct)
	if err != nil {
		return nil, "", err
	}
	return encRes.Body, encRes.URLOverride, nil
}

// ResponseCanonicalToIngressEmbeddings converts canonical OpenAI
// /v1/embeddings response JSON back into the client ingress response shape.
//
//   - OpenAI / Azure ingress → identity (canonical IS OpenAI).
//   - Cohere ingress → cohere helper.
//   - Gemini / Vertex ingress → single vs batch dispatch on the
//     nexus.ext.gemini.batch flag stamped by [Bridge.IngressEmbeddingsToCanonical].
//     When the flag is absent (e.g. canonical body originated outside the
//     bridge) the helper falls back to data[] cardinality: 1 → single,
//     >1 → batch.
func (b *Bridge) ResponseCanonicalToIngressEmbeddings(ingress provcore.Format, canonical []byte) ([]byte, error) {
	if openAILike(ingress) {
		return canonical, nil
	}
	switch ingress {
	case provcore.FormatCohere:
		return cohere.CanonicalToEmbedResponse(canonical)
	case provcore.FormatGemini, provcore.FormatVertex:
		batch, present := gemini.CanonicalEmbeddingBatchFlag(canonical)
		if !present {
			// Fall back to cardinality.
			if dataLen := embedDataLen(canonical); dataLen > 1 {
				return gemini.CanonicalToBatchEmbedContentsResponse(canonical)
			}
			return gemini.CanonicalToEmbedContentResponse(canonical)
		}
		if batch {
			return gemini.CanonicalToBatchEmbedContentsResponse(canonical)
		}
		return gemini.CanonicalToEmbedContentResponse(canonical)
	default:
		return nil, fmt.Errorf("canonicalbridge: ingress format %q has no embeddings response codec", ingress)
	}
}

// ResponseAcrossFormats reshapes a response body from one wire shape to
// another via canonical chat.completion. Used by the cache HIT path when
// the cached entry's origin shape differs from the requesting ingress
// shape.
//
// from / to are canonical typology.WireShape values; each one encodes
// both the endpoint kind and the body format. Internally the bridge
// reverses each WireShape back to a (provcore.Format, typology.WireShape)
// pair via wireShapeToFormatEndpoint to dispatch the per-codec
// DecodeResponse(fromFormat, body) → canonical chat.completion, then
// ResponseCanonicalToIngress(toFormat, canonical) → target wire shape.
// If from == to the function short-circuits and returns body unchanged.
//
// Returns the reshaped body. Errors come from codec dispatch (no codec
// registered for the source wire shape) or from the underlying
// DecodeResponse / ResponseCanonicalToIngress; callers are expected to
// fall back to serving the original body and log a warning rather than
// failing the request (the body is still a parseable response — just in
// the wrong wire shape — which is strictly less broken than failing the
// HIT).
func (b *Bridge) ResponseAcrossFormats(from typology.WireShape, to typology.WireShape, body []byte) ([]byte, error) {
	if from == to {
		return body, nil
	}
	canonical, err := b.responseToCanonical(from, body)
	if err != nil {
		return nil, err
	}
	toFormat, _, ok := wireShapeToFormatEndpoint(to)
	if !ok {
		return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: no format mapping for to wire-shape %q", to)
	}
	return b.ResponseCanonicalToIngress(toFormat, canonical)
}

// responseToCanonical projects a wire-shape response body to canonical
// chat-completions JSON. Centralises the dispatch so ResponseAcrossFormats
// handles ingress-only formats (WireShapeOpenAIResponses, whose codec
// lives in openai.responses and is addressed by endpoint, not format)
// without sprinkling special cases.
func (b *Bridge) responseToCanonical(from typology.WireShape, body []byte) ([]byte, error) {
	if from == typology.WireShapeOpenAIResponses {
		canon, _, derr := openai.DecodeResponsesResponse(body)
		if derr != nil {
			return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: decode responses-api→canonical: %w", derr)
		}
		if len(canon) == 0 {
			return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: responses-api decoder produced empty canonical body")
		}
		return canon, nil
	}
	fromFormat, fromEndpoint, ok := wireShapeToFormatEndpoint(from)
	if !ok {
		return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: no format mapping for from wire-shape %q", from)
	}
	codec, ok := b.codecs[fromFormat]
	if !ok || codec == nil {
		return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: no codec for from-format %q", fromFormat)
	}
	// Cross-format reshape decodes a previously-served response without the
	// originating request — request-relative codec checks fail open via a
	// zero DecodeContext.
	decodeRes, err := codec.DecodeResponse(fromEndpoint, body, "", provcore.DecodeContext{})
	if err != nil {
		return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: decode %s→canonical: %w", fromFormat, err)
	}
	if len(decodeRes.CanonicalBody) == 0 {
		return nil, fmt.Errorf("canonicalbridge: ResponseAcrossFormats: %s decoder produced empty canonical body", fromFormat)
	}
	return decodeRes.CanonicalBody, nil
}

// wireShapeToFormatEndpoint reverses a canonical typology.WireShape to
// the (provcore.Format, typology.WireShape) pair that the codec registry
// (b.codecs) is keyed on and that the resolved codec's DecodeResponse
// dispatches on. The returned WireShape is the endpoint value the target
// codec expects to recognise the body as a decodable response (e.g. the
// Gemini codec rejects anything that is not gemini-generate-content /
// vertex-generate-content; the Cohere codec requires cohere-chat).
//
// Coverage is the reverse of the forward egress map [ResponseCanonicalToIngress]
// (chat) and [ResponseCanonicalToIngressEmbeddings] (embeddings) plus every
// origin wire shape a cache entry may be persisted under — so the cross-shape
// cache-HIT reshape path ([ResponseAcrossFormats]) can decode any stored
// response back to canonical regardless of which upstream produced it.
// TestWireShapeToFormatEndpoint_CoversForwardEgressMap asserts this parity.
//
// Returns ok=false only for surfaces the bridge has no chat/embeddings
// canonical pipeline for (audio / image / batches; the bedrock-invoke raw
// passthrough shape).
func wireShapeToFormatEndpoint(w typology.WireShape) (provcore.Format, typology.WireShape, bool) {
	switch w {
	// --- Chat wire shapes ---
	case typology.WireShapeOpenAIChat:
		return provcore.FormatOpenAI, typology.WireShapeOpenAIChat, true
	case typology.WireShapeOpenAIResponses:
		return provcore.FormatOpenAIResponses, typology.WireShapeOpenAIResponses, true
	case typology.WireShapeOpenAICompletionsLegacy:
		return provcore.FormatOpenAI, typology.WireShapeOpenAICompletionsLegacy, true
	case typology.WireShapeAnthropicMessages:
		// Anthropic codec's DecodeResponse requires the anthropic-messages
		// endpoint; passing any other shape makes it passthrough the native
		// body uncanonicalised.
		return provcore.FormatAnthropic, typology.WireShapeAnthropicMessages, true
	case typology.WireShapeGeminiGenerateContent:
		return provcore.FormatGemini, typology.WireShapeGeminiGenerateContent, true
	case typology.WireShapeVertexGenerateContent:
		return provcore.FormatVertex, typology.WireShapeVertexGenerateContent, true
	case typology.WireShapeCohereChat:
		return provcore.FormatCohere, typology.WireShapeCohereChat, true
	case typology.WireShapeBedrockConverse:
		// Bedrock's codec ignores the endpoint for non-embed shapes and
		// delegates to the Anthropic codec internally; the converse endpoint
		// is the canonical chat shape for the Bedrock format.
		return provcore.FormatBedrock, typology.WireShapeBedrockConverse, true
	// --- Embeddings wire shapes ---
	case typology.WireShapeOpenAIEmbeddings:
		return provcore.FormatOpenAI, typology.WireShapeOpenAIEmbeddings, true
	case typology.WireShapeGeminiEmbedContent:
		return provcore.FormatGemini, typology.WireShapeGeminiEmbedContent, true
	case typology.WireShapeVertexEmbedContent:
		return provcore.FormatVertex, typology.WireShapeVertexEmbedContent, true
	case typology.WireShapeCohereEmbed:
		return provcore.FormatCohere, typology.WireShapeCohereEmbed, true
	case typology.WireShapeBedrockEmbeddings:
		return provcore.FormatBedrock, typology.WireShapeBedrockEmbeddings, true
	case typology.WireShapeVoyageEmbeddings:
		return provcore.FormatVoyage, typology.WireShapeVoyageEmbeddings, true
	}
	return "", "", false
}

// ResponseCanonicalToIngress converts canonical OpenAI chat.completion response
// JSON into the client's ingress response shape.
func (b *Bridge) ResponseCanonicalToIngress(ingress provcore.Format, canonical []byte) ([]byte, error) {
	if openAILike(ingress) {
		return canonical, nil
	}
	switch ingress {
	case provcore.FormatAnthropic:
		return anthropic.OpenAIChatCompletionToMessagesResponse(canonical)
	case provcore.FormatGemini, provcore.FormatVertex:
		return gemini.OpenAIChatCompletionToGenerateContentResponse(canonical)
	case provcore.FormatOpenAIResponses:
		// /v1/responses egress shape: modelOverride empty so the canonical
		// body's `model` field surfaces unchanged; requestID empty triggers
		// a synth id when the canonical lacks nexus.ext.openai.responses.id.
		return openai.EncodeResponsesResponse(canonical, "", "")
	default:
		return nil, fmt.Errorf("canonicalbridge: ingress format %q has no response hub codec", ingress)
	}
}

// SelfCheck walks every [Bridge.ChatRoutable] pair and asserts the bridge can
// produce upstream wire bytes via [Bridge.IngressChatToWire]. Intended to run
// at process startup so the routing matrix cannot drift from codec coverage.
func (b *Bridge) SelfCheck() error {
	formats := provcore.AllFormats()
	for _, ingress := range formats {
		body, err := MinimalNativeChatBody(ingress)
		if err != nil {
			continue
		}
		for _, target := range formats {
			if !b.ChatRoutable(ingress, target) {
				continue
			}
			ct := provcore.CallTarget{
				Format:          target,
				ProviderModelID: FixtureProviderModel(target),
			}
			if _, err := b.IngressChatToWire(ingress, target, body, ct, false); err != nil {
				return fmt.Errorf("canonicalbridge: %s→%s unusable: %w", ingress, target, err)
			}
		}
	}
	return nil
}
