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

	// EndpointKindVideoGeneration is a placeholder for provider
	// video-generation endpoints. Reserved before any provider ships one
	// in production; consumers should treat it as "valid kind, no rules
	// match yet" until a provider lands.
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
)

// AllEndpointKinds is the closed enumeration of every defined
// EndpointKind value. Tests assert exhaustiveness against this slice;
// consumers can iterate it for validation or UI population.
var AllEndpointKinds = []EndpointKind{
	EndpointKindChat,
	EndpointKindEmbeddings,
	EndpointKindImageGeneration,
	EndpointKindTTS,
	EndpointKindSTT,
	EndpointKindVideoGeneration,
	EndpointKindBatch,
	EndpointKindJob,
	EndpointKindModels,
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
