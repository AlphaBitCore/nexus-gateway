package typology

// EndpointKind is the semantic category of an HTTP traffic event — Axis 1
// of the typology. Values are typed string constants whose wire format
// is stable: every consumer (hook filter, routing matcher, cost
// estimator, traffic_event column, Prometheus label) reads the same
// strings.
//
// Adding a new value is a coordinated change across every consumer above.
type EndpointKind string

// EndpointKind constants. The string values are the canonical wire
// format — never rename a constant value without coordinating the rename
// across DB columns, Prometheus labels, MQ wire formats, and any
// downstream analytics SQL.
const (
	// EndpointKindChat covers conversational text generation:
	// /v1/chat/completions, /v1/messages (Anthropic), /v1/responses
	// (OpenAI Responses API), /v1/completions (legacy), Gemini
	// generateContent, Bedrock Converse. All are "the user wants a model
	// to produce a response to a prompt" regardless of wire shape.
	EndpointKindChat EndpointKind = "chat"

	// EndpointKindEmbeddings covers vector-embedding endpoints:
	// /v1/embeddings (OpenAI), /v1/embed (Cohere), Gemini embedContent,
	// Vertex *:embedContent / *:batchEmbedContents, Voyage embeddings.
	// Plural form — matches the deployed wire string at AI Gateway cost
	// formula registry / proxy dispatch / audit persistence / Prometheus
	// labels (see git grep "embeddings" packages/ai-gateway/). Renaming
	// to singular would silently break runtime cost-formula lookup; if
	// the rename is desired it is a Phase 3 DB-schema-migration concern.
	EndpointKindEmbeddings EndpointKind = "embeddings"

	// EndpointKindRerank covers document-reranking endpoints: /v1/rerank
	// (canonical, Cohere-shaped), Cohere /v2/rerank, Voyage /v1/rerank, and
	// provider-specific rerank endpoints. Given a query + candidate documents,
	// the model returns the documents re-ordered by relevance. The canonical
	// wire shape is Cohere's (not OpenAI's) — OpenAI ships no rerank API; see
	// provider-adapter-architecture.md §3a for the documented exception.
	EndpointKindRerank EndpointKind = "rerank"

	// EndpointKindImageGeneration covers image synthesis endpoints:
	// /v1/images/generations, /v1/images/edits, /v1/images/variations
	// (OpenAI), and provider-specific image-gen endpoints.
	EndpointKindImageGeneration EndpointKind = "image_generation"

	// EndpointKindTTS covers text-to-speech: /v1/audio/speech (OpenAI)
	// and provider-specific TTS endpoints.
	EndpointKindTTS EndpointKind = "tts"

	// EndpointKindSTT covers speech-to-text: /v1/audio/transcriptions
	// and /v1/audio/translations (OpenAI), plus provider-specific STT
	// endpoints.
	EndpointKindSTT EndpointKind = "stt"

	// EndpointKindVideoGeneration covers the async video-generation job
	// family: POST /v1/videos (submit) plus its body-less poll / content /
	// delete siblings, and provider-specific video-generation endpoints.
	EndpointKindVideoGeneration EndpointKind = "video_generation"

	// EndpointKindBatch covers async batch endpoints: /v1/batches
	// (OpenAI) and provider-specific batch ingest endpoints.
	EndpointKindBatch EndpointKind = "batch"

	// EndpointKindJob covers long-running provider job endpoints:
	// Bedrock InvokeModelAsync, Vertex prediction jobs, and similar.
	EndpointKindJob EndpointKind = "job"

	// EndpointKindModels covers the catalog-read endpoints (/v1/models,
	// /v1/models/{model}). Never carries user content; hook pipeline
	// and cost layer ignore these.
	EndpointKindModels EndpointKind = "models"

	// EndpointKindResponses is the OpenAI Responses API (/v1/responses). It is
	// a chat-FAMILY endpoint for routing / cache / hook purposes (the wire-shape
	// dispatch KindFromWireShape keeps it chat-kind), but carries its own
	// endpoint_type label so analytics and the route simulator can distinguish
	// Responses traffic from /v1/chat/completions.
	EndpointKindResponses EndpointKind = "responses"

	// EndpointKindGuardrail is the standalone compliance-verdict endpoint
	// (/v1/guardrail): a caller submits text and receives an allow/block/redact
	// verdict from the deployment's configured compliance pipeline (rule-pack +
	// PII + AI-Guard judge), without relaying an LLM completion. It carries no
	// provider wire shape (WireShapeNone) because nothing is routed upstream, and
	// it does not participate in the routing / executor / cost-formula / cache
	// path — it runs the hook pipeline directly and returns the verdict.
	EndpointKindGuardrail EndpointKind = "guardrail"

	// EndpointKindRealtime is the realtime voice session family
	// (GET /v1/realtime, WebSocket upgrade): a long-lived bidirectional
	// relay of the OpenAI Realtime API. A session emits one traffic_event
	// row per in-band response plus a session row, all labeled with this
	// kind. WireShapeNone — the upgrade request carries no HTTP body and no
	// codec ever sees realtime frames (verbatim relay).
	EndpointKindRealtime EndpointKind = "realtime"
)

// AllEndpointKinds is the closed enumeration of every defined
// EndpointKind value. Tests assert exhaustiveness against this slice;
// consumers can iterate it for validation or UI population.
var AllEndpointKinds = []EndpointKind{
	EndpointKindChat,
	EndpointKindEmbeddings,
	EndpointKindRerank,
	EndpointKindImageGeneration,
	EndpointKindTTS,
	EndpointKindSTT,
	EndpointKindVideoGeneration,
	EndpointKindBatch,
	EndpointKindJob,
	EndpointKindModels,
	EndpointKindResponses,
	EndpointKindGuardrail,
	EndpointKindRealtime,
}

// EndpointKindAcceptsModelType reports whether a catalog model of the given
// type (Model.type: chat/embedding/image/audio/tts/stt/realtime/video/rerank)
// can serve a
// request classified as this EndpointKind. It is the routing layer's modality
// guard: every routing strategy (single, loadbalance, conditional, latency,
// smart, fallback) and the requested-model passthrough resolve their targets
// through this, so a request is never dispatched to a model of the wrong
// modality — e.g. an image model selected for /v1/chat/completions, or a chat
// model auto-routed onto /v1/images/generations. The catalog `type` vocabulary
// is coarser than the endpoint vocabulary. Each audio endpoint accepts BOTH the
// fine type (tts / stt / realtime) and the coarse `audio` type, so a catalog
// that types its audio models precisely gets clean cross-sub-modality rejection
// (a stt model on the tts endpoint is refused) while a catalog that still types
// them coarsely as `audio` keeps routing — the coarse type is the back-compat
// fallback, the fine type the forward path (the shipped catalog types its
// audio models precisely). Video generation likewise accepts both `video`
// (the catalog's Sora type) and `image` (back-compat for rows created before
// the video type existed). Kinds that do not
// resolve to a catalog model (guardrail, batch, job, models) impose no
// constraint and accept any type. An empty modelType is the caller's signal to
// apply no constraint (see the resolver's modality filter), so it is accepted
// here too rather than guessed.
func EndpointKindAcceptsModelType(k EndpointKind, modelType string) bool {
	if modelType == "" {
		return true
	}
	switch k {
	case EndpointKindChat, EndpointKindResponses:
		return modelType == "chat"
	case EndpointKindEmbeddings:
		return modelType == "embedding"
	case EndpointKindImageGeneration:
		return modelType == "image"
	case EndpointKindTTS:
		return modelType == "tts" || modelType == "audio"
	case EndpointKindSTT:
		return modelType == "stt" || modelType == "audio"
	case EndpointKindRealtime:
		return modelType == "realtime" || modelType == "audio"
	case EndpointKindVideoGeneration:
		return modelType == "video" || modelType == "image"
	case EndpointKindRerank:
		return modelType == "rerank"
	default:
		return true
	}
}

// IsValid reports whether k is one of the defined EndpointKind constants.
// The empty EndpointKind is treated as invalid here — callers that need
// "unclassified" semantics check for empty string separately.
func (k EndpointKind) IsValid() bool {
	for _, valid := range AllEndpointKinds {
		if k == valid {
			return true
		}
	}
	return false
}

// String makes EndpointKind satisfy fmt.Stringer trivially.
func (k EndpointKind) String() string { return string(k) }
