package specutil

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// PtrInt returns a pointer to v. Used to populate the pointer-based
// [provcore.Usage] fields when a provider reports a value.
func PtrInt(v int) *int {
	return &v
}

// UsageFromOpenAI builds a canonical [provcore.Usage] from the
// standard OpenAI-style usage object:
//
//	{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
//
// Missing fields become nil pointers (not reported) rather than
// zero-valued pointers.
func UsageFromOpenAI(prompt, completion, total int, hasPrompt, hasCompletion, hasTotal bool) provcore.Usage {
	var u provcore.Usage
	if hasPrompt {
		u.PromptTokens = PtrInt(prompt)
	}
	if hasCompletion {
		u.CompletionTokens = PtrInt(completion)
	}
	if hasTotal {
		u.TotalTokens = PtrInt(total)
	}
	return u
}

// cachedTokenAliases is the ordered list of usage-object paths a known
// OpenAI-compat upstream may use to report "cached prompt tokens".
// First match wins (most-canonical → most-quirky). New families landing
// in this gateway append here rather than re-implementing the chain in
// every codec. Each entry pairs the JSON path with the upstream we
// observed using it — keep that observation accurate so future
// debugging traces the alias back to its source.
var cachedTokenAliases = []struct {
	path     string
	upstream string // for comments / debugging only; not used in matching
}{
	{"prompt_tokens_details.cached_tokens", "OpenAI canonical (2024-09+)"},
	{"input_tokens_details.cached_tokens", "OpenAI Responses API (/v1/responses)"},
	{"prompt_cache_hit_tokens", "DeepSeek"},
	{"prompt_cache_tokens", "Moonshot explicit-cache API"},
	{"cached_tokens", "Kimi K2 / K2.5 / K2.6 auto-prefix cache"},
}

// reasoningTokenAliases is the same idea for reasoning / thinking
// token counts. OpenAI o-series, DeepSeek-reasoner, and Moonshot
// k2-thinking currently all share the same canonical path; add a row
// when a new family diverges.
var reasoningTokenAliases = []struct {
	path     string
	upstream string
}{
	{"completion_tokens_details.reasoning_tokens", "OpenAI o-series / DeepSeek-reasoner / Moonshot kimi-k2-thinking"},
	{"output_tokens_details.reasoning_tokens", "OpenAI Responses API (/v1/responses)"},
}

// ExtractCacheReadTokens resolves the canonical "cached prompt tokens"
// value from any of the [cachedTokenAliases] paths on the given usage
// gjson object. Returns nil when none of the known aliases is present.
//
// Callers: every adapter SchemaCodec.DecodeResponse that consumes an
// OpenAI-compat upstream MUST call this (not re-implement the alias
// chain). The d914275a-era audit found that scattered alias logic was
// the root cause of cache tokens silently disappearing from
// traffic_event whenever a new sibling provider was wired without
// reviewing every alias.
func ExtractCacheReadTokens(u gjson.Result) *int {
	for _, a := range cachedTokenAliases {
		if v := u.Get(a.path); v.Exists() {
			return PtrInt(int(v.Int()))
		}
	}
	return nil
}

// ExtractReasoningTokens resolves the canonical "reasoning tokens"
// value from any of the [reasoningTokenAliases] paths.
func ExtractReasoningTokens(u gjson.Result) *int {
	for _, a := range reasoningTokenAliases {
		if v := u.Get(a.path); v.Exists() {
			return PtrInt(int(v.Int()))
		}
	}
	return nil
}

// ExtractOpenAIUsage extracts a fully-populated [provcore.Usage] from
// the standard OpenAI-compat usage object. Combines prompt /
// completion / total + cached + reasoning. Missing fields stay nil
// (not zero-valued). Shared by openai.identityCodec and any
// OpenAI-compat sibling that writes its own codec.
//
// Handles both top-level token-count field names:
//   - chat-completions: prompt_tokens / completion_tokens / total_tokens
//   - Responses API:    input_tokens  / output_tokens     / total_tokens
//
// First match wins per field; an upstream that reports both names (which
// no observed upstream does today) collapses to the chat-completions
// reading.
func ExtractOpenAIUsage(u gjson.Result) provcore.Usage {
	if !u.Exists() {
		return provcore.Usage{}
	}
	usage := provcore.Usage{
		CacheReadTokens: ExtractCacheReadTokens(u),
		ReasoningTokens: ExtractReasoningTokens(u),
	}
	if v := u.Get("prompt_tokens"); v.Exists() {
		usage.PromptTokens = PtrInt(int(v.Int()))
	} else if v := u.Get("input_tokens"); v.Exists() {
		usage.PromptTokens = PtrInt(int(v.Int()))
	}
	if v := u.Get("completion_tokens"); v.Exists() {
		usage.CompletionTokens = PtrInt(int(v.Int()))
	} else if v := u.Get("output_tokens"); v.Exists() {
		usage.CompletionTokens = PtrInt(int(v.Int()))
	}
	if v := u.Get("total_tokens"); v.Exists() {
		usage.TotalTokens = PtrInt(int(v.Int()))
	}
	return usage
}
