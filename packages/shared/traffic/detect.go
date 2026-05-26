package traffic

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// RequestMeta is the per-request LLM signal extracted from an intercepted
// HTTP request. Populated by Adapter.DetectRequestMeta. Empty-string fields
// mean "unknown" or "not applicable" — never nil semantically.
type RequestMeta struct {
	// Provider is a stable provider identifier (e.g. "openai", "anthropic",
	// "gemini", "bedrock"). Empty when the request does not match any
	// known provider.
	Provider string

	// Model is the model string parsed from the request (e.g. "gpt-4o-mini",
	// "claude-3-5-sonnet-20241022"). Empty when absent from the body.
	Model string

	// Path is the canonical provider-relative path (e.g. "/v1/chat/completions").
	// Useful when the adapter serves multiple endpoints and downstream code
	// wants to branch on it without re-parsing.
	Path string

	// ApiKeyClass is a coarse label of the presented API key.
	// Stable set: "sk-ant-", "sk-proj-", "sk-", "AIza", "nvk_",
	// "azure-api-key", "aws-sigv4", "gcp-oauth", "glm-jwt", "" (unknown).
	ApiKeyClass string

	// ApiKeyFingerprint is SHA256(key)[:8] as hex (16 chars).
	// Empty when no key was extractable from the request.
	ApiKeyFingerprint string
}

// UsageStatus is the per-response status describing how token usage was
// obtained (or why it was not). Matches the CHECK constraint on
// traffic_event.usage_extraction_status.
type UsageStatus string

const (
	// UsageStatusOK — non-streaming response with a fully parsed usage block.
	UsageStatusOK UsageStatus = "ok"

	// UsageStatusStreamingReported — streaming response where the provider
	// emitted a terminal usage frame (Anthropic message_stop, Gemini final
	// usage, OpenAI with stream_options.include_usage).
	UsageStatusStreamingReported UsageStatus = "streaming_reported"

	// UsageStatusStreamingEstimated — streaming response without provider
	// usage; counts come from the shared tokenizer (Tier 2).
	UsageStatusStreamingEstimated UsageStatus = "streaming_estimated"

	// UsageStatusStreamingUnavailable — streaming response where neither
	// the provider nor the tokenizer could produce counts (including
	// >10MB Agent MITM bypass).
	UsageStatusStreamingUnavailable UsageStatus = "streaming_unavailable"

	// UsageStatusParseFailed — AI-shaped response whose body could not be
	// parsed into a known usage structure.
	UsageStatusParseFailed UsageStatus = "parse_failed"

	// UsageStatusNoBody — response had no inspectable body (HEAD, 204,
	// connection terminated before bytes flowed).
	UsageStatusNoBody UsageStatus = "no_body"

	// UsageStatusNonLLM — traffic did not match any AI provider adapter;
	// counts are irrelevant.
	UsageStatusNonLLM UsageStatus = "non_llm"
)

// UsageMeta is the per-response usage signal. Populated by
// Adapter.DetectResponseUsage. Token pointers are nil when unavailable.
//
// CacheReadTokens and ReasoningTokens mirror the canonical OpenAI splits
// (usage.prompt_tokens_details.cached_tokens and
// usage.completion_tokens_details.reasoning_tokens) and are set by codecs
// that observe a vendor cache hit (Anthropic cache_read_input_tokens,
// Gemini cachedContentTokenCount, DeepSeek prompt_cache_hit_tokens) or a
// reasoning-token report (Gemini thoughtsTokenCount, OpenAI o1/o3,
// DeepSeek-reasoner). Cost analytics use them to price the cache /
// reasoning split separately.
//
// CacheCreationTokens is the write-side cache cost counter. Only populated
// by providers that report it (Anthropic: cache_creation_input_tokens).
type UsageMeta struct {
	PromptTokens        *int
	CompletionTokens    *int
	CacheReadTokens        *int
	ReasoningTokens     *int
	CacheCreationTokens *int
	Status              UsageStatus
}

// RequestDetector is the request-side detector capability. Implementors
// MUST be safe for concurrent use — one Adapter instance is shared across
// all requests routed to it.
type RequestDetector interface {
	// DetectRequestMeta extracts LLM signals from an intercepted request.
	// Never returns an error: failure to extract yields an empty-string
	// field (or ApiKeyClass == "") and must not propagate to the caller.
	DetectRequestMeta(r *http.Request, body []byte) RequestMeta
}

// ResponseUsageDetector is the response-side detector capability.
// Implementors MUST be safe for concurrent use.
type ResponseUsageDetector interface {
	// DetectResponseUsage extracts token counts from an intercepted
	// response. For streaming responses, callers should consult the
	// streaming accumulator instead; DetectResponseUsage is for the
	// non-streaming path.
	DetectResponseUsage(r *http.Response, body []byte) UsageMeta
}

// ApiKeyFingerprint returns SHA256(key)[:8] as a 16-char lowercase hex
// string. Returns "" when key is empty. Stable, deterministic, non-reversible.
func ApiKeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// ApiKeyClassify returns the stable class label for a presented key.
// Returns "" when the key is empty or does not match any known prefix.
// The caller is responsible for extracting the raw key from the request
// (Authorization header, x-api-key, x-goog-api-key, …) before calling this.
func ApiKeyClassify(key string) string {
	switch {
	case key == "":
		return ""
	case strings.HasPrefix(key, "sk-ant-"):
		return "sk-ant-"
	case strings.HasPrefix(key, "sk-proj-"):
		return "sk-proj-"
	case strings.HasPrefix(key, "sk-"):
		return "sk-"
	case strings.HasPrefix(key, "AIza"):
		return "AIza"
	case strings.HasPrefix(key, "nvk_"):
		return "nvk_"
	}
	return ""
}

// ExtractBearerToken returns the token portion of an Authorization: Bearer
// header. Returns "" when absent or not in Bearer form.
func ExtractBearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}
