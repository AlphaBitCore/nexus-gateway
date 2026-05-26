package canonicalbridge

// CanonicalVersion is the internal identifier for the hub JSON contract:
// OpenAI Chat Completions as of the 2024-10 API generation, plus the
// extensions listed in [CanonicalRequestSubset] and [CanonicalResponseSubset].
const CanonicalVersion = "openai.chat.completions.2024-10"

// CanonicalRequestSubset enumerates fields a translation codec may read or
// emit on canonical **request** bodies. Paths use dot notation for nesting.
// Anything outside this set must use [GetExt]/[SetExt] under nexus.ext or be
// rejected with CodeInvalidRequest when unmappable.
//
// Required:
//   - model
//   - messages (non-empty array; each element: role, content)
//   - roles: system, user, assistant, tool; content is string or part array
//   - tool messages carry content as a string plus tool_call_id (NOT a
//     tool_result content part — that shape is Anthropic-native and is
//     produced by the Anthropic codec only).
//
// Optional (canonical OpenAI subset):
//   - temperature, top_p, max_tokens, max_completion_tokens
//   - stop (string or array of strings)
//   - stream, stream_options.include_usage
//   - tools (OpenAI shape: type function, function.name, function.description, function.parameters)
//   - tool_choice (none | auto | required | object with type function and function.name)
//   - response_format (json_object or json_schema with json_schema)
//   - parallel_tool_calls
//   - seed, user, logit_bias
//   - logprobs (boolean), top_logprobs (integer 0..5)
//   - metadata (flat map string to string; <=16 pairs, key <=64ch, value <=512ch)
//   - service_tier (auto | default | flex | priority)
//   - nexus.ext.<provider>.* (passthrough namespace)
//
// Optional (non-OpenAI extensions; specific codecs honour them):
//   - top_k — Anthropic, Gemini, Bedrock-Anthropic. OpenAI rejects this
//     field; senders targeting an OpenAI-shape adapter receive a 400
//     from upstream. Hub policy: forward as-is, rely on the target API
//     for validation.
//
// Request content part types (inside messages[].content when array):
//   - type text + text
//   - type image_url + image_url.url, image_url.detail (auto|low|high)
//
// max_tokens vs max_completion_tokens: OpenAI deprecated max_tokens in
// favour of max_completion_tokens for o1/o3-style reasoning models
// (max_tokens is silently ignored on those endpoints). Codecs accept
// either; canonical preserves whichever the client supplied.
const CanonicalRequestSubset = "see SubsetFields request list"

// CanonicalResponseSubset enumerates fields a translation codec may read or
// emit on canonical **response** bodies (non-streaming).
//
// Required:
//   - id, object (chat.completion), created, model
//   - choices[].index, choices[].message, choices[].finish_reason
//   - message.role assistant, message.content (string or null if only tool_calls)
//   - finish_reason in stop, length, tool_calls, content_filter, function_call
//     (function_call is legacy; emitted only for the deprecated
//     functions/function_call request path, not the modern tools API)
//
// Optional:
//   - message.tool_calls (id, type function, function.name, function.arguments)
//   - message.refusal (string|null) — populated by structured-outputs and
//     o1/o3 models when the assistant declines to comply
//   - usage.prompt_tokens, usage.completion_tokens, usage.total_tokens
//   - usage.prompt_tokens_details.cached_tokens
//   - usage.prompt_tokens_details.audio_tokens
//   - usage.completion_tokens_details.reasoning_tokens
//   - logprobs, system_fingerprint, service_tier
//   - nexus.ext.<provider>.* (passthrough namespace)
//
// OpenAI-documented response extensions: tool_calls, refusal, logprobs,
// usage.prompt_tokens_details.cached_tokens,
// usage.prompt_tokens_details.audio_tokens,
// usage.completion_tokens_details.reasoning_tokens.
const CanonicalResponseSubset = "see SubsetFields response list"

// CanonicalStreamChunkSubset enumerates fields on streaming chat.completion.chunk
// envelopes.
//
//   - id, object (chat.completion.chunk), created, model
//   - choices[].index, choices[].delta, choices[].finish_reason (nullable)
//   - delta.role (first chunk), delta.content
//   - delta.refusal (structured outputs / reasoning models)
//   - delta.tool_calls[].index, id, function.name, function.arguments (fragments)
//   - service_tier, system_fingerprint (chunk-level passthrough)
//   - trailing usage chunk when stream_options.include_usage; then data: [DONE]
const CanonicalStreamChunkSubset = "see SubsetFields stream list"

// SubsetFields returns flat JSON path lists for the canonical hub: request
// body keys, response body keys, and stream chunk keys. Used by self-check
// scaffolding and documentation consumers.
func SubsetFields() (request, response, stream []string) {
	request = []string{
		"model",
		"messages",
		"temperature",
		"top_p",
		"top_k", // non-OpenAI extension; honoured by Anthropic/Gemini/Bedrock codecs
		"max_tokens",
		"max_completion_tokens", // OpenAI reasoning-model successor to max_tokens
		"stop",
		"stream",
		"stream_options",
		"stream_options.include_usage",
		"tools",
		"tool_choice",
		"response_format",
		"parallel_tool_calls",
		"seed",
		"user",
		"logit_bias",
		"logprobs",
		"top_logprobs",
		"metadata",
		"service_tier",
		"nexus.ext",
	}
	response = []string{
		"id",
		"object",
		"created",
		"model",
		"choices",
		"choices[].index",
		"choices[].message",
		"choices[].message.role",
		"choices[].message.content",
		"choices[].message.refusal",
		"choices[].message.tool_calls",
		"choices[].finish_reason",
		"usage",
		"usage.prompt_tokens",
		"usage.completion_tokens",
		"usage.total_tokens",
		"usage.prompt_tokens_details",
		"usage.prompt_tokens_details.cached_tokens",
		"usage.prompt_tokens_details.audio_tokens",
		"usage.completion_tokens_details",
		"usage.completion_tokens_details.reasoning_tokens",
		"logprobs",
		"system_fingerprint",
		"service_tier",
		"nexus.ext",
	}
	stream = []string{
		"id",
		"object",
		"created",
		"model",
		"choices",
		"choices[].index",
		"choices[].delta",
		"choices[].delta.role",
		"choices[].delta.content",
		"choices[].delta.refusal",
		"choices[].delta.tool_calls",
		"choices[].finish_reason",
		"service_tier",
		"system_fingerprint",
		"usage",
	}
	return request, response, stream
}
