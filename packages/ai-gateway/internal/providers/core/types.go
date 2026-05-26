// Package core defines the declarative adapter surface that the AI
// Gateway uses to talk to upstream LLM providers.
//
// The top-level public contract is the three-method [Adapter] interface.
// Each provider is assembled from four smaller components ([Transport],
// [SchemaCodec], [StreamDecoder], [ErrorNormalizer]) composed into an
// [AdapterSpec] and wrapped by the generic specAdapter in the sibling
// dispatch package.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Format is the provider wire format. The set is one-to-one with the
// non-fallback IDs in shared/traffic/adapters. The traffic adapter
// named "generic-jsonpath" is a traffic-side fallback only; it has no
// provider-side counterpart and is intentionally absent here.
type Format string

const (
	FormatOpenAI      Format = "openai"
	FormatDeepSeek    Format = "deepseek"
	FormatGLM         Format = "glm"
	FormatAzureOpenAI Format = "azure-openai"
	FormatAnthropic   Format = "anthropic"
	FormatGemini      Format = "gemini"
	FormatMiniMax     Format = "minimax"
	FormatBedrock     Format = "bedrock"
	FormatVertex      Format = "vertex"
	FormatCohere      Format = "cohere"
	FormatHuggingFace Format = "huggingface"
	FormatReplicate   Format = "replicate"
	// OpenAI-compat re-users — distinct Format constants so vendor-scoped
	// audit, metrics, and rate-limit policies can target them without
	// pattern-matching on Provider name. Each ships as a thin spec_X
	// package that delegates wire encoding/decoding to openai.
	FormatMistral    Format = "mistral"
	FormatXai        Format = "xai"
	FormatGroq       Format = "groq"
	FormatPerplexity Format = "perplexity"
	FormatTogether   Format = "together"
	FormatFireworks  Format = "fireworks"
	FormatMoonshot   Format = "moonshot"
	FormatVoyage     Format = "voyage"

	// FormatOpenAIResponses is OpenAI's /v1/responses wire format, the
	// distinct request/response shape for reasoning models + built-in tools
	// + server-side conversation state. Treated as a sibling ingress format,
	// NOT a new canonical: the canonical bus remains OpenAI chat-completions
	// shape per provider-adapter-architecture.md §3a Rule 1. The /v1/responses
	// codec under spec_openai translates in both directions; same-shape
	// passthrough is gated by the target adapter's RequestShapes containing
	// "responses-api".
	FormatOpenAIResponses Format = "openai-responses"
)

// AllFormats returns every declared chat-completions wire [Format] in
// stable order. Callers that want to iterate the chat-routing matrix
// (registry seeding, canonical-bridge self-check, cross-pair matrix
// tests) should use this helper. Endpoint-scoped formats that live
// outside the chat-completions matrix — currently FormatOpenAIResponses
// (which serves EndpointResponsesAPI, not EndpointChatCompletions) —
// are intentionally NOT included here; they are still .Valid() and
// still detected at the route layer.
func AllFormats() []Format {
	return []Format{
		FormatOpenAI,
		FormatDeepSeek,
		FormatGLM,
		FormatAzureOpenAI,
		FormatAnthropic,
		FormatGemini,
		FormatMiniMax,
		FormatBedrock,
		FormatVertex,
		FormatCohere,
		FormatHuggingFace,
		FormatReplicate,
		FormatMistral,
		FormatXai,
		FormatGroq,
		FormatPerplexity,
		FormatTogether,
		FormatFireworks,
		FormatMoonshot,
		FormatVoyage,
	}
}

// Valid reports whether f is a known format.
func (f Format) Valid() bool {
	switch f {
	case FormatOpenAI, FormatDeepSeek, FormatGLM, FormatAzureOpenAI,
		FormatAnthropic, FormatGemini, FormatMiniMax, FormatBedrock, FormatVertex,
		FormatCohere, FormatHuggingFace, FormatReplicate,
		FormatMistral, FormatXai, FormatGroq, FormatPerplexity,
		FormatTogether, FormatFireworks, FormatMoonshot, FormatVoyage,
		FormatOpenAIResponses:
		return true
	}
	return false
}

// IsOpenAIFamily reports whether bodies in this format share the
// canonical OpenAI chat completions JSON schema — model at the JSON
// root, messages array, etc. — so that simple `payload["model"] = X`
// substitution works as a passthrough rewrite.
//
// Must stay in sync with the set of formats that wire spec_openai's
// IdentityCodec as their SchemaCodec; specAdapter.rewritePassthroughModel
// uses this method to gate model-field rewriting before upstream
// dispatch. Keeping the list here (rather than open-coded per call site)
// is what makes routing to a Moonshot/Mistral/Groq/... target carry the
// target's ProviderModelID instead of the originator's model code.
func (f Format) IsOpenAIFamily() bool {
	switch f {
	case FormatOpenAI, FormatDeepSeek, FormatGLM, FormatAzureOpenAI,
		FormatMoonshot, FormatMiniMax, FormatHuggingFace,
		FormatMistral, FormatXai, FormatGroq, FormatPerplexity,
		FormatTogether, FormatFireworks:
		return true
	}
	return false
}

// CallTarget is the fully-resolved upstream target for a single call.
// Populated by an implementation of [github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target.Resolver]
// and then passed through [Adapter.Execute]. Adapters must not mutate it.
type CallTarget struct {
	ProviderID   string
	ProviderName string // stable slug ("openai", "anthropic", ...)
	// Format is the wire adapter the gateway uses to talk to this
	// provider. Sourced from the Provider.adapter_type column (one of
	// the nine canonical providers.Format values). The executor, smart
	// router, AI Guard, and handler call sites read this field instead
	// of re-deriving the adapter from ProviderName.
	Format          Format
	BaseURL         string // no trailing slash
	APIKey          string // plaintext after vault decrypt
	CredentialID    string // UUID of the Credential row used; empty when key comes from provider config
	CredentialName  string // human-readable name of the credential
	ProviderModelID string // vendor's model ID (e.g. "claude-3-5-sonnet-20241022")

	// Extras carries provider-specific configuration that doesn't fit in
	// the universal fields above. Keys are dot-namespaced: "azure.apiVersion",
	// "aws.accessKey", "gcp.serviceAccountJSON", etc.
	Extras map[string]string
}

// Get returns the Extras value for key, or "" when absent.
func (t CallTarget) Get(key string) string {
	if t.Extras == nil {
		return ""
	}
	return t.Extras[key]
}

// Request is the input to [Adapter.Execute]. Body is raw bytes in
// BodyFormat — either the canonical OpenAI shape (BodyFormat=FormatOpenAI)
// or a vendor-native body when the ingress route is native. Adapters
// pass-through when BodyFormat == Adapter.Format(), and otherwise ask
// their SchemaCodec to translate.
type Request struct {
	WireShape  typology.WireShape
	BodyFormat Format
	Body       []byte
	Headers    http.Header // filtered, safe-to-forward subset (Authorization stripped)
	Stream     bool
	Target     CallTarget

	// StickyKey is an opaque discriminator (typically the virtual key ID)
	// used by the credential pool selector for consistent hashing so the
	// same caller always routes to the same credential and maximises
	// provider-side prompt-cache hits. Empty = weighted-random fallback.
	StickyKey string

	// MaxResponseBytes bounds the bytes the adapter will read from a
	// non-streaming upstream response. Set per-request from the runtime
	// payload-capture config (`MaxResponseBytes`, default 10 MiB) so
	// admin edits take effect on the next request without a restart.
	// A non-positive value falls back to ReadAllLimit at the adapter so
	// a stale or zeroed config never collapses the read to zero (which
	// would surface as an empty upstream response). Streaming responses
	// are bounded by shared/streaming policies and are not affected by
	// this field.
	MaxResponseBytes int64
}

// Response is the output from [Adapter.Execute]. For non-streaming
// calls, Body holds the full canonical response bytes and Stream is nil.
// For streaming calls, Body is nil and Stream is non-nil.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte        // populated iff !Stream
	Stream     StreamSession // populated iff Stream
	Usage      Usage
	BodyFormat Format // native format on the wire from the provider
	// TargetMethod + TargetPath capture the URL the adapter dispatched
	// to upstream — the basis for traffic_event.target_method /
	// target_path. Empty for synthetic errors that never reached the
	// network (the handler falls back to client method/path).
	TargetMethod string
	TargetPath   string
	// Coerced lists any in-place request rewrites the adapter applied before
	// dispatching upstream, formatted as "<from>→<to>". Populated for example
	// when applyOpenAIReasoningRewrites renamed max_tokens for a reasoning model.
	// Used by the gateway handler to emit x-nexus-coerced. Empty when no
	// rewrite occurred.
	Coerced []string
}

// Usage is the canonical token-accounting envelope.
//
// The canonical struct lives in shared/normalize so that the AI Gateway,
// the compliance proxy, the agent, and the Hub audit pipeline all consume
// the same definition. The Go alias keeps existing ai-gateway code that
// writes `providers.Usage` compiling unchanged.
//
// Field semantics (canonical source: shared/normalize/types.go):
//   - PromptTokens: total input tokens (OpenAI convention = uncached +
//     cached_read + cached_write). Anthropic's raw input_tokens is
//     normalized to this convention inside the Anthropic Tier-1
//     normalizer; do not subtract again at the call site.
//   - CompletionTokens: total output tokens including reasoning tokens.
//   - TotalTokens: PromptTokens + CompletionTokens.
//   - CacheReadTokens: read-side cache hit (Anthropic cache_read_input_tokens,
//     OpenAI prompt_tokens_details.cached_tokens / input_tokens_details.cached_tokens,
//     Gemini cachedContentTokenCount, Kimi flat cached_tokens, DeepSeek
//     prompt_cache_hit_tokens, Moonshot prompt_cache_tokens).
//   - CacheCreationTokens: write-side cache surcharge (Anthropic
//     cache_creation_input_tokens). Other providers leave nil.
//   - ReasoningTokens: thinking subset of CompletionTokens (OpenAI
//     completion_tokens_details.reasoning_tokens / Responses
//     output_tokens_details.reasoning_tokens, Gemini thoughtsTokenCount).
type Usage = normcore.Usage

// StreamSession is a push-less streaming cursor. Callers drive it with
// repeated [StreamSession.Next] calls; on [io.EOF] the stream is
// complete and Close must be invoked to release upstream resources.
type StreamSession interface {
	// Next returns the next decoded chunk. io.EOF signals end of stream.
	// RawBytes on each chunk is the provider-native SSE/NDJSON frame,
	// forwardable to the client without re-wrapping.
	Next(ctx context.Context) (Chunk, error)
	Close() error
}

// Chunk is one decoded streaming event.
type Chunk struct {
	Delta          string          // text delta (assistant content), canonical UTF-8
	ReasoningDelta string          // reasoning / thinking text (Anthropic thinking_delta, OpenAI / DeepSeek delta.reasoning_content). Kept separate from Delta so audit / hooks aggregate only assistant-visible content.
	ToolCallDeltas []ToolCallDelta // partial tool call updates (OpenAI shape)
	Usage          *Usage          // set when provider emits usage mid-stream or at end
	Done           bool            // terminal chunk (equivalent to provider's "[DONE]" / message_stop)
	RawBytes       []byte          // provider-native bytes (SSE frame incl. "data: " prefix, or NDJSON line)
	NativeEvent    string          // optional provider event name (e.g. "message_delta")
}

// ToolCallDelta is a partial OpenAI-canonical tool call patch within a
// streamed Chunk. Index identifies which tool call slot this delta belongs to.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// ProbeResult is the outcome of [Adapter.Probe].
type ProbeResult struct {
	OK        bool
	LatencyMs int64
	Detail    string
	Err       error
}

// ProviderError is the canonical error envelope returned by any
// [Adapter.Execute] or [Adapter.Probe] that encountered an upstream
// failure. Code is drawn from a small canonical set so callers can
// branch on a stable string without reading the provider's Type.
type ProviderError struct {
	Status     int
	Code       string // canonical: "invalid_request", "auth_failed", "rate_limited", "timeout", "upstream_error", "endpoint_unsupported", "not_implemented", "no_compatible_provider"
	Type       string // provider's own type string, preserved for observability
	Message    string
	RetryAfter *time.Duration
	Raw        []byte      // provider error payload verbatim
	Headers    http.Header // upstream response headers, cloned; nil for synthetic errors that never reached the network
	// TargetMethod + TargetPath capture the URL the adapter dispatched
	// to upstream — same semantics as Response.TargetMethod / TargetPath.
	// Set for 4xx/5xx that actually reached the network; empty for
	// synthetic errors (timeout, transport).
	TargetMethod string
	TargetPath   string
}

// Error implements the error interface with a "<code>: <message>"
// surface suitable for logs.
func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Canonical error codes surfaced on [ProviderError.Code]. Adapters must
// use these exactly; new codes require a single-line addition here so
// callers have a single source of truth.
const (
	CodeInvalidRequest       = "invalid_request"
	CodeAuthFailed           = "auth_failed"
	CodeRateLimited          = "rate_limited"
	CodeTimeout              = "timeout"
	CodeUpstreamError        = "upstream_error"
	CodeEndpointUnsupported  = "endpoint_unsupported"
	CodeNotImplemented       = "not_implemented"
	CodeNoCompatibleProvider = "no_compatible_provider"
)

// ReadAllLimit is the conservative upper bound for reading a provider
// response body when no per-request cap is supplied (e.g. health
// probes, error-body sniffing on a 4xx). Mirrors
// payloadcapture.DefaultMaxResponseBytes.
const ReadAllLimit = 10 * 1024 * 1024

// LimitedReadAll is a convenience wrapper used by Transport
// implementations when reading a non-streaming response body where no
// runtime cap is plumbed through (probe / error-body paths).
func LimitedReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, ReadAllLimit))
}

// LimitedReadAllN is the runtime-cap variant used on the request hot
// path. The cap is plumbed from Request.MaxResponseBytes; values <= 0
// fall back to ReadAllLimit so a malformed or unset
// payload-capture row never collapses the read to zero.
func LimitedReadAllN(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = ReadAllLimit
	}
	return io.ReadAll(io.LimitReader(r, max))
}

// EmbeddingsInput is the canonical embedding input discriminator.
// Exactly one of String / Strings / Tokens is populated per valid request;
// MarshalJSON/UnmarshalJSON enforce this contract on the wire boundary.
type EmbeddingsInput struct {
	String  *string
	Strings []string
	Tokens  [][]int
}

// Wire shape decision: a single-element token batch ({Tokens: [][]int{{1,2,3}}})
// MARSHALS as a bare [1,2,3] array, identical to what UnmarshalJSON would
// produce for that wire shape. This keeps the round-trip lossless: encode →
// decode yields the same in-memory shape. The downside is asymmetric in the
// other direction: a client that explicitly sends [[1,2,3]] (one batch entry)
// will be re-emitted as [1,2,3]. Document this as the chosen contract; any
// downstream that needs to detect "explicit-batch-of-one" must inspect the
// raw wire bytes before canonicalization.

// MarshalJSON encodes EmbeddingsInput back to the JSON wire shape:
//   - single string                  → bare JSON string
//   - []string                       → JSON array of strings
//   - [][]int with exactly one entry → JSON array of integers (single token sequence)
//   - [][]int with multiple entries  → JSON array of integer arrays (batch)
//   - zero value                     → JSON null (should not occur in valid usage)
//
// The single-element flatten keeps the wire shape symmetric with
// UnmarshalJSON, which decodes `[1,2,3]` into `Tokens: [][]int{{1,2,3}}`.
func (e EmbeddingsInput) MarshalJSON() ([]byte, error) {
	switch {
	case e.String != nil:
		return json.Marshal(*e.String)
	case e.Strings != nil:
		return json.Marshal(e.Strings)
	case e.Tokens != nil:
		if len(e.Tokens) == 1 {
			return json.Marshal(e.Tokens[0])
		}
		return json.Marshal(e.Tokens)
	default:
		return []byte("null"), nil
	}
}

// UnmarshalJSON decodes the four legal OpenAI embedding input shapes:
//   - bare string        → String field
//   - array of strings   → Strings field
//   - array of integers  → Tokens[0] (single token array)
//   - array of arrays    → Tokens field
//   - anything else      → error
func (e *EmbeddingsInput) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	// Try string scalar first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.String = &s
		return nil
	}
	// Try raw array — need to inspect element type.
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("embeddings input: expected string or array, got: %s", data)
	}
	if len(raw) == 0 {
		e.Strings = []string{}
		return nil
	}
	// Peek at the first element to discriminate.
	first := raw[0]
	// Check if first element is an integer.
	var firstInt int
	if err := json.Unmarshal(first, &firstInt); err == nil {
		// Array of integers → single token sequence.
		tokens := make([]int, len(raw))
		for i, elem := range raw {
			if err := json.Unmarshal(elem, &tokens[i]); err != nil {
				return fmt.Errorf("embeddings input tokens[%d]: %w", i, err)
			}
		}
		e.Tokens = [][]int{tokens}
		return nil
	}
	// Check if first element is an array of integers.
	var firstArr []int
	if err := json.Unmarshal(first, &firstArr); err == nil {
		// Array of token arrays.
		tokensBatch := make([][]int, len(raw))
		for i, elem := range raw {
			if err := json.Unmarshal(elem, &tokensBatch[i]); err != nil {
				return fmt.Errorf("embeddings input tokens[%d]: %w", i, err)
			}
		}
		e.Tokens = tokensBatch
		return nil
	}
	// Check if first element is a string → array of strings.
	var firstStr string
	if err := json.Unmarshal(first, &firstStr); err == nil {
		strs := make([]string, len(raw))
		for i, elem := range raw {
			if err := json.Unmarshal(elem, &strs[i]); err != nil {
				return fmt.Errorf("embeddings input strings[%d]: %w", i, err)
			}
		}
		e.Strings = strs
		return nil
	}
	return fmt.Errorf("embeddings input: unrecognised element type in array: %s", first)
}

// EmbeddingsRequest is the canonical request envelope for /v1/embeddings.
// Field semantics follow the OpenAI Embeddings API shape; all providers
// translate from this representation inside their embedding codec.
type EmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          EmbeddingsInput `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat *string         `json:"encoding_format,omitempty"`
	User           *string         `json:"user,omitempty"`
}

// EmbeddingDataItem is one embedding vector in an EmbeddingsResponse.
// Base64 carries the raw base64 string when encoding_format="base64";
// it is NOT rendered in JSON (json:"-") because callers read it from the
// raw upstream body; the gateway forwards the provider response verbatim
// on the embedding path so this field exists for internal post-processing
// only.
type EmbeddingDataItem struct {
	Object    string    `json:"object"`     // "embedding"
	Embedding []float32 `json:"embedding,omitempty"`
	Base64    string    `json:"-"`
	Index     int       `json:"index"`
}

// EmbeddingsUsage is the token-usage envelope returned with every
// EmbeddingsResponse. PromptTokens = input tokens consumed by the
// embedding model; TotalTokens = PromptTokens (no completion tokens for
// embeddings).
type EmbeddingsUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// EmbeddingsResponse is the canonical response envelope for /v1/embeddings.
// Data holds one EmbeddingDataItem per input string / token sequence.
type EmbeddingsResponse struct {
	Object string              `json:"object"` // "list"
	Data   []EmbeddingDataItem `json:"data"`
	Model  string              `json:"model"`
	Usage  EmbeddingsUsage     `json:"usage"`
}

// ArtifactKind identifies the media type of an ArtifactRef.
type ArtifactKind string

const (
	ArtifactKindImage ArtifactKind = "image"
	ArtifactKindAudio ArtifactKind = "audio"
	ArtifactKindVideo ArtifactKind = "video"
	ArtifactKindJob   ArtifactKind = "job"
)

// ArtifactRef is a reference to an opaque media artifact produced by an
// upstream provider (image URL, audio bytes, video URL, async job).
// Always nil/zero for chat and embedding codecs.
type ArtifactRef struct {
	Kind      ArtifactKind
	MIMEType  string
	URL       string
	Bytes     []byte
	Base64    string
	JobID     string
	Width     int
	Height    int
	DurationS float64
	SizeBytes int64
}

// JobStatus is the lifecycle state of an async provider job.
type JobStatus string

const (
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCanceled  JobStatus = "canceled"
)

// JobRef identifies an asynchronous provider job submitted via
// EncodeRequest and polled via the job-status endpoint.
type JobRef struct {
	ProviderID  string
	JobID       string
	InternalID  string
	SubmittedAt time.Time
}
