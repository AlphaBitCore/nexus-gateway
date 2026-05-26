package openai

import (
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// DetectRequestMeta extracts provider/model/api-key signals from an OpenAI
// or OpenAI-compatible request. Never errors — missing fields return empty
// strings.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "openai"}
	if r != nil {
		meta.Path = r.URL.Path
	}

	if len(body) > 0 && gjson.ValidBytes(body) {
		if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
			meta.Model = m.Str
		}
	}

	key := traffic.ExtractBearerToken(r)
	if key != "" {
		meta.ApiKeyClass = traffic.ApiKeyClassify(key)
		meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(key)
	}
	return meta
}

// DetectResponseUsage parses a non-streaming OpenAI chat/completions or
// embeddings response for token usage. For streaming responses callers
// should consult the streaming accumulator instead.
func (a *Adapter) DetectResponseUsage(_ *http.Response, body []byte) traffic.UsageMeta {
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	if !gjson.ValidBytes(body) {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}

	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}

	var um traffic.UsageMeta
	um.Status = traffic.UsageStatusOK
	if p := usage.Get("prompt_tokens"); p.Exists() && p.Type == gjson.Number {
		v := int(p.Int())
		um.PromptTokens = &v
	}
	if c := usage.Get("completion_tokens"); c.Exists() && c.Type == gjson.Number {
		v := int(c.Int())
		um.CompletionTokens = &v
	}
	// responses API uses input_tokens/output_tokens instead — support either.
	if um.PromptTokens == nil {
		if p := usage.Get("input_tokens"); p.Exists() && p.Type == gjson.Number {
			v := int(p.Int())
			um.PromptTokens = &v
		}
	}
	if um.CompletionTokens == nil {
		if c := usage.Get("output_tokens"); c.Exists() && c.Type == gjson.Number {
			v := int(c.Int())
			um.CompletionTokens = &v
		}
	}
	// OpenAI canonical splits (cache + reasoning). Mirrors what
	// spec_openai.IdentityCodec.DecodeResponse surfaces on
	// providers.Usage so cost analytics see identical signals through
	// either path. DeepSeek's flat `prompt_cache_hit_tokens` is treated
	// as a synonym for cached_tokens when the OpenAI shape is missing
	// (DeepSeek's chat.completions wire format pre-dates OpenAI's
	// prompt_tokens_details rollout).
	if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.CacheReadTokens = &n
	} else if v := usage.Get("prompt_cache_hit_tokens"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.CacheReadTokens = &n
	}
	if v := usage.Get("completion_tokens_details.reasoning_tokens"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.ReasoningTokens = &n
	}
	return um
}
