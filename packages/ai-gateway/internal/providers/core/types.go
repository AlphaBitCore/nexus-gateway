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
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"sync"
	"time"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

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

	// ServesResponsesAPI is the per-provider override for whether this
	// upstream natively serves OpenAI /v1/responses. nil = adapter
	// RequestShapes default; an explicit value is downgrade-only (false
	// forces canonical(chat); true cannot exceed the adapter capability).
	// Resolved once per target in the executor failover loop from the
	// hydrated routing snapshot — never a per-request DB read.
	ServesResponsesAPI *bool

	// MaxOutputTokens is the model's advertised output ceiling from the
	// catalog (0 when unset) — the same number /v1/models reports. An adapter
	// whose wire requires an output cap fills/clamps against THIS value rather
	// than a private table, so a wrong number is one bug instead of a silent
	// divergence between what we advertise and what we send upstream.
	MaxOutputTokens int

	// Reasons: this model thinks before answering. The catalogue records it;
	// until now nothing carried it here, so an egress codec could not ask
	// whether the model it was about to call reasons at all — and a codec that
	// cannot ask sends a reasoning parameter to every model or to none.
	//
	// The flag alone does not make a correct translation possible: a budget
	// wire also needs that model's minimum and maximum, and a level wire needs
	// its legal levels, none of which the catalogue carries yet. It is the part
	// that is knowable today.
	Reasons bool

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
	// Truncated is set when the non-streaming response body could not be
	// read in full before usage extraction — either the upstream body
	// exceeded the runtime read cap (LimitedReadAllN / MaxResponseBytes) or a
	// compressed body exceeded the decompressed-size bound. A truncated body
	// means any usage block parsed from it is incomplete, so the handler must
	// stamp usage_extraction_status="truncated" rather than "ok" — billing and
	// analytics must never treat a partial buffer as a confirmed usage block.
	// Always false for streaming responses (captured-body
	// truncation there is tracked separately via Record.ResponseTruncated and
	// does not affect provider-reported stream usage).
	Truncated bool
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
	// NexusThinking carries a complete Anthropic thinking block only after its
	// provider signature has arrived. It is an opaque exact-replay carrier;
	// ordinary reasoning text is never enough to synthesize a native block.
	NexusThinking []NexusThinkingBlock
	Usage         *Usage // set when provider emits usage mid-stream or at end
	Done          bool   // terminal chunk (equivalent to provider's "[DONE]" / message_stop)
	RawBytes      []byte // provider-native bytes (SSE frame incl. "data: " prefix, or NDJSON line)
	NativeEvent   string // optional provider event name (e.g. "message_delta")
	// FinishReason carries the canonical OpenAI finish_reason ("stop",
	// "length", "tool_calls", "content_filter", ...) once the provider's
	// stream signals completion. Each stream decoder maps its wire's
	// stop/finish token into this canonical vocabulary on the frame that
	// carries it (often NOT the terminal Done frame — OpenAI reports it on a
	// trailing delta-empty chunk, Anthropic on message_delta). Empty until
	// observed. Canonical re-encoders (buffer mode, cross-format transcode)
	// read it so a reconstructed stream preserves the real finish_reason
	// instead of collapsing to "stop"; an empty value defaults to "stop" at
	// the encoder for backward compatibility.
	FinishReason string
	// Truncated rides on the single terminal chunk that the broker
	// non-stream leader synthesises from a buffered ExecutionResult: it
	// propagates Response.Truncated so a leader whose response body was
	// clamped at the read cap fans out the truncation signal to every
	// joiner, which then stamp usage_extraction_status="truncated" rather
	// than "ok". Unused on genuine streaming chunks.
	Truncated bool
	// Verbatim marks a chunk whose RawBytes are already in the client's
	// egress wire shape and MUST be forwarded byte-for-byte, not re-encoded.
	// Set by the /v1/responses content copier for a genuine-Responses upstream
	// so built-in-tool / audio events the canonical waist cannot represent
	// survive. The live relay honours it only on the non-enforced passthrough
	// lane; an enforcing scope forces the canonical buffer (decoded fields).
	// Default false → existing decoders unaffected.
	Verbatim bool
}

// ToolCallDelta is a partial OpenAI-canonical tool call patch within a
// streamed Chunk. Index identifies which tool call slot this delta belongs to.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
	// ThoughtSignature is Gemini's provider-native signature for this exact
	// functionCall Part. It is omitted when unsigned.
	ThoughtSignature string
}

// NexusThinkingBlock is the typed representation of the existing
// `nexus_thinking` per-message extension. Index is stream-local and is not
// serialized; Thinking/Signature/RedactedData replay the native block.
type NexusThinkingBlock struct {
	Index        int    `json:"-"`
	Thinking     string `json:"thinking,omitempty"`
	Signature    string `json:"signature,omitempty"`
	RedactedData string `json:"redacted_data,omitempty"`
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
	CodeInvalidRequest = "invalid_request"
	CodeAuthFailed     = "auth_failed"
	CodeRateLimited    = "rate_limited"
	// CodeProviderQuotaExhausted is the provider ACCOUNT's budget being spent,
	// not a rate limit and not a bad request. It is account-scoped, so every
	// model behind that provider is equally unusable until the window resets
	// or the customer raises the limit — which means the request should move
	// to a different provider, and the credential should not be penalised for
	// a key that is perfectly valid.
	CodeProviderQuotaExhausted = "provider_quota_exhausted"
	CodeTimeout                = "timeout"
	CodeUpstreamError          = "upstream_error"
	CodeEndpointUnsupported    = "endpoint_unsupported"
	// CodeContextOverflow: prompt exceeds the model's window; the executor fails over to a larger target, never same-target retry.
	CodeContextOverflow      = "context_overflow"
	CodeNotImplemented       = "not_implemented"
	CodeNoCompatibleProvider = "no_compatible_provider"
	// CodeClientGone: the caller's context was cancelled, so the transport
	// error says nothing about the provider. Distinct from CodeTimeout
	// because the two demand opposite reactions — a timeout is evidence
	// against the upstream and a cancellation is evidence about the client.
	// Collapsing them recorded a health failure against an innocent provider
	// every time a user pressed stop, and a cancellation storm could push
	// every provider it touched past the unavailability threshold.
	CodeClientGone = "client_gone"
	// CodeLocalProcessing: the upstream answered 2xx — and billed for it —
	// and WE failed afterwards, reading or decoding what it sent. Reporting
	// that as an upstream error made it retryable, and every retry is a fresh
	// billed generation for a request that was already paid for once. The
	// upstream is not at fault and cannot fix it by being asked again.
	CodeLocalProcessing = "local_processing_failed"
	// CodeSpendLimitExceeded is OUR ceiling, not the upstream's: a request
	// asking for more billable units than the gateway will multiply from one
	// call (image `n`, rerank documents). Distinct from CodeInvalidRequest,
	// which is the body being wrong — this body is well-formed and the
	// provider would have served it. Naming it separately is what lets an
	// operator group spend refusals apart from codec faults, and lets a caller
	// tell "I asked for too many" from "you could not translate my request".
	//
	// Gateway-internal, so it stays OUT of CanonicalCodes and out of the Hub's
	// shared vocabulary — for the same reason CodeClientGone and
	// CodeLocalProcessing do. That slice enumerates the codes an ADAPTER
	// produces from an upstream response, which is what the Hub's
	// upstream-vs-gateway alert split keys on. No adapter can ever emit this
	// one: the request never reached an upstream.
	// The VALUE is UPPER_SNAKE, unlike its lower_snake neighbours, because it
	// is the only code here that reaches a CALLER through the gateway's own
	// error envelope rather than through a normalised provider error — and
	// that surface is UPPER_SNAKE by a contract sdk_compat pins.
	CodeSpendLimitExceeded = "SPEND_LIMIT_EXCEEDED"
)

// CanonicalCodes enumerates every code above. It exists so a code added to the
// const block cannot quietly skip the surfaces that have to know about it —
// the shared vocabulary the Hub reads, and the upstream/gateway split its
// alert rules key on. The contract test counts against this slice, so a new
// constant that is not added here fails to compile the intent rather than
// passing silently, which is how provider_upstream_error spent its whole
// production life counting nothing.
var CanonicalCodes = []string{
	CodeInvalidRequest,
	CodeAuthFailed,
	CodeRateLimited,
	CodeProviderQuotaExhausted,
	CodeTimeout,
	CodeUpstreamError,
	CodeEndpointUnsupported,
	CodeContextOverflow,
	CodeNotImplemented,
	CodeNoCompatibleProvider,
}

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

// respReadBufPool reuses the scratch buffer LimitedReadAllN reads the upstream
// non-streaming response into. io.ReadAll starts at 512 B and regrows
// geometrically, allocating ~2x the body size in throwaway intermediate slices
// per response (measured churn on the hot path); a pooled buffer pre-grown to a
// typical body size absorbs that growth and is reused. The body still escapes to
// the decoder + async audit, so a single right-sized copy is taken out and the
// scratch is returned to the pool — the same pattern as the request-side
// readBody pool.
var respReadBufPool = sync.Pool{New: func() any {
	b := new(bytes.Buffer)
	b.Grow(64 << 10)
	return b
}}

// respReadBufPoolCap bounds the capacity a buffer may have and still be returned
// to the pool: one oversized response must not inflate every pooled scratch
// thereafter.
const respReadBufPoolCap = 1 << 20 // 1 MiB

// LimitedReadAllN is the runtime-cap variant used on the response hot
// path. The cap is plumbed from Request.MaxResponseBytes; values <= 0
// fall back to ReadAllLimit so a malformed or unset payload-capture row
// never collapses the read to zero.
//
// It reads up to max+1 bytes so an oversize body is *detectable*: when the
// upstream sends more than the cap, the returned slice is clamped to max
// and truncated=true tells the caller the bytes are incomplete — any usage
// block parsed from them cannot be trusted. truncated is
// always false on the error path and whenever the body fit within the cap.
func LimitedReadAllN(r io.Reader, max int64) (data []byte, truncated bool, err error) {
	if max <= 0 {
		max = ReadAllLimit
	}
	buf := respReadBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		// Do not pool back a buffer that ballooned on an oversized response.
		if buf.Cap() <= respReadBufPoolCap {
			respReadBufPool.Put(buf)
		}
	}()
	if _, err = buf.ReadFrom(io.LimitReader(r, max+1)); err != nil {
		return nil, false, err
	}
	// Right-sized escaping copy; the pooled scratch is reused next response.
	if int64(buf.Len()) > max {
		return append([]byte(nil), buf.Bytes()[:max]...), true, nil
	}
	return append([]byte(nil), buf.Bytes()...), false, nil
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
	Object    string    `json:"object"` // "embedding"
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
