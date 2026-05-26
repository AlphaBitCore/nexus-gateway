package core

import "encoding/json"

// Kind discriminates the structural shape carried by a NormalizedPayload.
type Kind string

const (
	KindAIChat        Kind = "ai-chat"
	KindAICompletion  Kind = "ai-completion"
	KindAIEmbedding   Kind = "ai-embedding"
	KindAIImage       Kind = "ai-image"
	KindHTTPJSON      Kind = "http-json"
	KindHTTPText      Kind = "http-text"
	KindHTTPForm      Kind = "http-form"
	KindHTTPMultipart Kind = "http-multipart"
	KindHTTPBinary    Kind = "http-binary"
	KindUnsupported   Kind = "unsupported"
)

// IsAI reports whether k is one of the ai-* kinds.
func (k Kind) IsAI() bool {
	switch k {
	case KindAIChat, KindAICompletion, KindAIEmbedding, KindAIImage:
		return true
	}
	return false
}

// IsHTTP reports whether k is one of the http-* kinds.
func (k Kind) IsHTTP() bool {
	switch k {
	case KindHTTPJSON, KindHTTPText, KindHTTPForm, KindHTTPMultipart, KindHTTPBinary:
		return true
	}
	return false
}

// Direction indicates which side of an exchange the payload represents.
type Direction string

const (
	DirectionRequest  Direction = "request"
	DirectionResponse Direction = "response"
)

// Role enumerates the speaker of an AI message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentType enumerates the structural variants inside a ContentBlock.
type ContentType string

const (
	ContentText       ContentType = "text"
	ContentImageRef   ContentType = "image_ref"
	ContentToolUse    ContentType = "tool_use"
	ContentToolResult ContentType = "tool_result"
	ContentReasoning  ContentType = "reasoning"
)

// NormalizedPayload is the canonical representation of one captured
// request or response. Discriminated by Kind. Producers fill only the
// fields relevant to the chosen Kind.
//
// JSON tags use camelCase to match the OpenAPI schema served by the
// admin API; the same shape is persisted to traffic_event_normalized.
type NormalizedPayload struct {
	Kind             Kind   `json:"kind"`
	NormalizeVersion string `json:"normalizeVersion"`
	Protocol         string `json:"protocol,omitempty"`

	// AI fields — populated when Kind.IsAI().
	Model        string         `json:"model,omitempty"`
	Stream       bool           `json:"stream,omitempty"`
	Messages     []Message      `json:"messages,omitempty"`
	Tools        []ToolDef      `json:"tools,omitempty"`
	Params       *SamplingParam `json:"params,omitempty"`
	Usage        *Usage         `json:"usage,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`

	// Inputs carries the text input(s) for KindAIEmbedding request payloads.
	// Populated by embedding normalizers on the request side only; stays nil
	// on the response side (embedding responses contain only float vectors,
	// which are intentionally NEVER stored in NormalizedPayload per SDD §T2.3).
	// Token-array inputs ([]int / [][]int) are not representable here; the
	// normalizer leaves Inputs nil and sets RuleIDs=["binary_input_token_array"]
	// as a marker so downstream consumers understand the omission.
	Inputs []string `json:"inputs,omitempty"`

	// HTTP fields — populated when Kind.IsHTTP().
	HTTP *HTTPPayload `json:"http,omitempty"`

	// Storage-only marker. When true, the payload was dropped per policy
	// (storageAction=drop-content) and only metadata is retained.
	Redacted bool     `json:"redacted,omitempty"`
	RuleIDs  []string `json:"ruleIds,omitempty"`

	// Confidence indicates how confident the producing normalizer is in
	// the structural fields it filled. Range [0, 1]. 0 (the JSON zero
	// value, omitempty-elided on the wire) is interpreted by the
	// Coordinator (Registry.Normalize) as "not reported" and treated as
	// fully confident (1.0). A normalizer that DOES report Confidence
	// signals to the Coordinator whether to fall through to the next
	// Tier; values >= the registry's tier threshold (default 0.7) keep
	// this payload as the final answer, lower values let pattern-based
	// extraction or verbatim fallback try next. New per-adapter
	// normalizers SHOULD set Confidence to reflect partial parse
	// quality (e.g. 0.85 when shape matched but some optional fields
	// were unrecognised).
	Confidence float64 `json:"confidence,omitempty"`

	// DetectedSpec records WHICH adapter or wire spec the producing
	// normalizer matched. Examples: "openai-chat", "anthropic-messages",
	// "gemini-generate" (Tier 1 adapter normalizers), "chatgpt-web",
	// "claude-web" (consumer-surface adapters), or
	// "pattern:openai-chat", "pattern:anthropic-messages" (Tier 2
	// pattern probe identifying the most likely spec). Used by the UI
	// to show a "detected as X" badge and by analytics to break down
	// audit volume by structural family across host names. Empty for
	// the verbatim Tier 3 fallback (no specific spec was identified).
	DetectedSpec string `json:"detectedSpec,omitempty"`
}

// Message is one element in NormalizedPayload.Messages.
// MarshalJSON guarantees Content serialises as `[]` and never `null` —
// reasoning-only responses (o1/o3, gemini-2.5-pro hitting max_tokens on
// thinking) and tool-only assistant turns can legitimately produce an
// empty content slice, and JS consumers iterating `.map()` over null
// would crash. The wire shape always carries an empty array.
type Message struct {
	Role         Role           `json:"role"`
	Content      []ContentBlock `json:"content"`
	FinishReason string         `json:"finishReason,omitempty"`
}

// MarshalJSON serialises Message with Content guaranteed non-null.
func (m Message) MarshalJSON() ([]byte, error) {
	if m.Content == nil {
		m.Content = []ContentBlock{}
	}
	type alias Message
	return json.Marshal(alias(m))
}

// ContentBlock is one structural element inside a Message.
type ContentBlock struct {
	Type       ContentType `json:"type"`
	Text       string      `json:"text,omitempty"`
	ImageRef   *BinaryRef  `json:"imageRef,omitempty"`
	ToolUse    *ToolUse    `json:"toolUse,omitempty"`
	ToolResult *ToolResult `json:"toolResult,omitempty"`
}

// BinaryRef references a binary blob (image, audio, file) by hash and
// size without inlining bytes. The blob itself may live in the spill
// store; SpillKey, when non-empty, addresses it.
type BinaryRef struct {
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
	SpillKey    string `json:"spillKey,omitempty"`
}

// ToolDef declares one tool exposed to the model.
type ToolDef struct {
	Name                 string         `json:"name"`
	Description          string         `json:"description,omitempty"`
	ParametersJSONSchema map[string]any `json:"parametersJsonSchema,omitempty"`
}

// ToolUse is the model's request to invoke a tool.
type ToolUse struct {
	CallID string         `json:"callId,omitempty"`
	Name   string         `json:"name"`
	Input  map[string]any `json:"input,omitempty"`
}

// ToolResult is the user/system response to a previous tool_use.
type ToolResult struct {
	CallID string `json:"callId,omitempty"`
	Output string `json:"output,omitempty"`
}

// SamplingParam carries the model sampling configuration.
type SamplingParam struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
	TopK        *int     `json:"topK,omitempty"`
	MaxTokens   *int     `json:"maxTokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// Usage carries token-count metadata.
type Usage struct {
	PromptTokens        *int `json:"promptTokens,omitempty"`
	CompletionTokens    *int `json:"completionTokens,omitempty"`
	TotalTokens         *int `json:"totalTokens,omitempty"`
	CacheReadTokens     *int `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens *int `json:"cacheCreationTokens,omitempty"`
	// ReasoningTokens is the subset of completion tokens the provider
	// reported as chain-of-thought / hidden reasoning (Gemini's
	// `reasoning_tokens` in completion_tokens_details, OpenAI o1/o3,
	// DeepSeek-reasoner, Moonshot kimi-thinking). When reasoning
	// consumed the whole `max_tokens` budget the visible content is
	// empty — surfacing the count lets audit readers explain it.
	ReasoningTokens *int `json:"reasoningTokens,omitempty"`
}

// HTTPPayload is the non-AI HTTP representation.
type HTTPPayload struct {
	Method          string            `json:"method,omitempty"`
	URL             string            `json:"url,omitempty"`
	HeadersFiltered map[string]string `json:"headersFiltered,omitempty"`
	BodyView        *HTTPBodyView     `json:"bodyView,omitempty"`
}

// HTTPBodyView carries the decoded body in the form most useful for the
// kind. Exactly one of Text / JSON / Form / BinaryRef is typically set
// per HTTPBodyView; producers may set Text alongside JSON to provide a
// pretty-printed projection for text-only consumers.
type HTTPBodyView struct {
	Text      string            `json:"text,omitempty"`
	JSON      any               `json:"json,omitempty"`
	Form      map[string]string `json:"form,omitempty"`
	BinaryRef *BinaryRef        `json:"binaryRef,omitempty"`
}

// TransformSource identifies the subsystem that produced a TransformSpan.
// Used for audit grouping ("show me everything cache_normaliser changed
// this week") and policy filtering (compliance officer reviewing only
// hook-attributable changes).
type TransformSource string

const (
	// SourceHook — content-touching hook redact (pii-detector / keyword-
	// filter / content-safety / rulepack-engine), attributed to a rule.
	SourceHook TransformSource = "hook"
	// SourceAIGuard — LLM-as-judge classifier suggested a redact span.
	// Distinguished from SourceHook so operators can audit AI-driven
	// modifications separately.
	SourceAIGuard TransformSource = "aiguard"
	// SourceCacheNormaliser — prompt-cache normaliser stripped volatile
	// bytes from the request body before sending upstream (helps the
	// provider's prompt-cache hit rate).
	SourceCacheNormaliser TransformSource = "cache-normaliser"
	// SourceCacheControlInject — cache_control marker injection
	// (Nexus added markers to direct the provider's prompt cache).
	SourceCacheControlInject TransformSource = "cache-control-inject"
	// SourceCacheKeyStrip — Nexus L1 cache-key normalisation removed
	// volatile bytes for the cache key computation only; upstream
	// body unaffected. Recorded for audit completeness.
	SourceCacheKeyStrip TransformSource = "cache-key-strip"
)

// TransformAction classifies what kind of edit a TransformSpan made.
type TransformAction string

const (
	ActionRedact  TransformAction = "redact"  // sensitive content replaced
	ActionStrip   TransformAction = "strip"   // volatile bytes removed
	ActionInject  TransformAction = "inject"  // bytes added
	ActionReplace TransformAction = "replace" // generic substitution
)

// TransformSpan describes one byte-level modification on a
// NormalizedPayload. Spans canonicalize every modification a Nexus
// subsystem made between the client and the upstream: hook redactions,
// AI-Guard suggestions, cache-normaliser strips, cache_control inject.
// A single span set drives both inflight rewrite (TrafficAdapter
// applies them to the upstream-bound body) and storage rewrite (the
// audit-log copy stored in traffic_event_normalized).
//
// Reconstructing the wire-level body from the audit log:
//
//	upstream_body = ApplySpans(request_normalized, request_transform_spans)
//	client_body   = ApplySpans(response_normalized, response_transform_spans)
//
// ContentAddress encodes the addressed content block:
//
//   - AI kinds: "messages.<i>.content.<j>" (e.g. "messages.0.content.1")
//   - HTTP kinds: "http.bodyView" (whole body) or "http.bodyView.form.<key>"
//
// Start / End are UTF-8 byte offsets into the addressed content's text.
// For ActionInject, Start == End and Replacement holds the injected bytes.
type TransformSpan struct {
	Source         TransformSource `json:"source"`
	SourceID       string          `json:"sourceId,omitempty"` // rule ID, hook ID, normaliser rule ID, or ""
	Action         TransformAction `json:"action"`
	ContentAddress string          `json:"contentAddress"`
	Start          int             `json:"start"`
	End            int             `json:"end"`
	Replacement    string          `json:"replacement,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

// RedactionSpan is retained as an alias for backward semantic clarity
// in narrow APIs (hook results), but new code should use TransformSpan
// directly so non-redact sources (cache normaliser, cache_control
// inject) flow through the same audit channel.
type RedactionSpan = TransformSpan
