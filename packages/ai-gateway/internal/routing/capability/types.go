// Package capability provides capability pre-filtering for the routing engine.
// It parses Model.capabilityJson into structured descriptors and applies
// compatibility rules before strategy dispatch, rejecting candidates whose
// model cannot honour the embedding request parameters.
//
package capability

import (
	"encoding/json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// EmbeddingsCapability is the parsed `capabilityJson.embeddings` block
// of a Model row, used by the routing pre-filter to reject candidates
// before strategy dispatch.
type EmbeddingsCapability struct {
	MaxInputTokens           int      `json:"max_input_tokens,omitempty"`
	SupportedDimensions      []int    `json:"supported_dimensions,omitempty"`
	DefaultDimension         int      `json:"default_dimension,omitempty"`
	MaxBatchSize             int      `json:"max_batch_size,omitempty"`
	SupportedEncodingFormats []string `json:"supported_encoding_formats,omitempty"`
	SupportedInputTypes      []string `json:"supported_input_types,omitempty"` // Cohere
	SupportedTaskTypes       []string `json:"supported_task_types,omitempty"`  // Gemini
	// RequiredExtensions lists nexus.ext.* keys that callers MUST set for
	// this model (e.g., `cohere.input_type` for Cohere v3, `gemini.taskType`
	// for Gemini text-embedding-004). Empty when no provider-mandated
	// extensions apply. The filter projects this list into
	// CandidateCapability.RequiredExtensions on rejection so the admin UI
	// can render WHY a routing candidate was unavailable.
	RequiredExtensions []string `json:"required_extensions,omitempty"`
}

// ModelCapability is the parsed capability descriptor for one Model row.
type ModelCapability struct {
	InputModalities  []string
	OutputModalities []string
	Lifecycle        string
	Embeddings       *EmbeddingsCapability // nil if model is not an embedding model
}

// EmbeddingRequest is the routing-side view of an embedding request,
// extracted from the canonical body by the proxy handler. Nil pointer fields
// mean "client omitted this field" and the model's default applies.
type EmbeddingRequest struct {
	Dimensions     *int   // nil = client omitted dimensions
	BatchSize      int    // 1 for single-input requests; len(input) for arrays
	EncodingFormat string // "" / "float" / "base64"
	InputType      string // nexus.ext.cohere.input_type (Cohere v3)
	TaskType       string // nexus.ext.gemini.taskType (Gemini)
}

// CandidateCapability describes what a routing candidate would have accepted.
// Used to populate available_capabilities in 400 errors when all candidates
// are rejected by the pre-filter.
type CandidateCapability struct {
	Provider                 string   `json:"provider"`
	Model                    string   `json:"model"`
	SupportedDimensions      []int    `json:"supported_dimensions,omitempty"`
	MaxBatchSize             int      `json:"max_batch_size,omitempty"`
	SupportedEncodingFormats []string `json:"supported_encoding_formats,omitempty"`
	RequiredExtensions       []string `json:"required_extensions,omitempty"`
}

// rawCapabilityDoc is the unmarshalling target for capabilityJson.
// Only the "embeddings" key is consumed by this package; future keys
// (e.g. "image", "audio") can be added without touching this code.
type rawCapabilityDoc struct {
	Embeddings *EmbeddingsCapability `json:"embeddings"`
}

// ParseModelCapability extracts a ModelCapability from a store.Model row.
// Returns nil when the Model has no capability data (CapabilityJson is nil).
// On JSON parse error, returns a non-nil ModelCapability with the modality
// and lifecycle fields populated but Embeddings == nil (fail-permissive for
// non-embedding endpoints; fail-restrictive for the embeddings pre-filter
// since a missing Embeddings block means no capability data was declared).
func ParseModelCapability(m *store.Model) *ModelCapability {
	if m == nil {
		return nil
	}
	cap := &ModelCapability{
		InputModalities:  m.InputModalities,
		OutputModalities: m.OutputModalities,
		Lifecycle:        m.Lifecycle,
	}
	if len(m.CapabilityJson) == 0 {
		return cap
	}
	var doc rawCapabilityDoc
	if err := json.Unmarshal(m.CapabilityJson, &doc); err != nil {
		// Malformed JSON: return capability with Embeddings == nil.
		// The pre-filter will treat this as "no capability data declared"
		// and reject the candidate on the embeddings path.
		return cap
	}
	cap.Embeddings = doc.Embeddings
	return cap
}
