package codecs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// OpenAIChatNormalizer handles OpenAI's /v1/chat/completions surface
// (both non-streaming JSON responses and SSE streamed responses).
//
// It also handles the legacy /v1/completions endpoint when KindForPath
// returns ai-completion — that variant is structurally the same but
// uses `prompt` instead of `messages`. Embeddings (/v1/embeddings) and
// image (/v1/images/generations) are handled by sibling normalizers
// registered under their own routing keys.
type OpenAIChatNormalizer struct{}

// NewOpenAIChatNormalizer returns a normalizer instance. The struct is
// stateless; one instance per process is sufficient.
func NewOpenAIChatNormalizer() *OpenAIChatNormalizer { return &OpenAIChatNormalizer{} }

// ID returns the identifier used in metrics labels and traffic_event
// diagnostics.
func (n *OpenAIChatNormalizer) ID() string { return "openai-chat" }

// LooksLike implements core.Sniffer: matches the OpenAI Chat wire by
// its distinctive markers, probed within the leading bytes only —
// an SSE stream whose first data payload is a chat-completion chunk
// (`data: {"id":"chatcmpl`), or a JSON body carrying the
// `"object":"chat.completion` discriminator (the closing quote is
// deliberately omitted so both "chat.completion" and
// "chat.completion.chunk" values match). The bare `data:` prefix the
// stream decoder accepts as a fallback is deliberately NOT enough
// here: every SSE protocol shares it, and matching it would steal
// Anthropic / Gemini streams from their own codecs.
//
// Request direction (or direction unset): a body carrying BOTH
// `"messages"` and `"model"` matches, with one negative marker —
// `"author"` must be absent. The chatgpt-web consumer request is the
// known messages+model collision: its messages[] items wrap the role
// in an `author` object (OpenAI Chat messages never carry that key)
// and `"messages"` opens the body, so the marker always sits inside
// the probe window. Without the exclusion, key-missed chatgpt-web
// requests would be claimed here with empty content instead of
// reaching the Tier-2 chatgpt-web spec that decodes them fully.
// Anthropic requests also carry messages+model; the sniff walk
// registers anthropic first and its stricter messages+max_tokens
// requirement wins before this probe runs.
func (n *OpenAIChatNormalizer) LooksLike(raw []byte, meta core.Meta) bool {
	probe := sniffProbe(raw)
	if bytes.Contains(probe, []byte(`"object":"chat.completion`)) {
		return true
	}
	if meta.Direction != core.DirectionResponse &&
		bytes.Contains(probe, []byte(`"messages"`)) &&
		bytes.Contains(probe, []byte(`"model"`)) &&
		!bytes.Contains(probe, []byte(`"author"`)) {
		return true
	}
	s := bytes.TrimLeft(probe, " \t\r\n")
	if rest, ok := bytes.CutPrefix(s, []byte("data:")); ok {
		return bytes.HasPrefix(bytes.TrimLeft(rest, " "), []byte(`{"id":"chatcmpl`))
	}
	return false
}

// Normalize routes by Meta.Direction to the request or response path.
// Empty raw bytes return core.ErrUnsupported so the caller records the
// failure cleanly.
func (n *OpenAIChatNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroPayloadForKind(meta), fmt.Errorf("openai-chat: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroPayloadForKind(meta), fmt.Errorf("openai-chat: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	// Confidence semantics (one meaning per input shape): a stream fold
	// computes frame coverage (recognized / total data frames) and sets
	// Confidence itself; single-document bodies score weighted field
	// coverage by comparing the wire body's top-level keys against the
	// openai-chat FieldSpec — see confidence.go for the scoring rubric.
	// Required absence (model/choices/usage missing) drives the score
	// below 0.70 and lets the Coordinator fall through to Tier-2;
	// unknown keys (new OpenAI optional fields, vendor extensions)
	// apply a bounded −0.10 penalty.
	if err == nil {
		if p.Confidence == 0 {
			p.Confidence = core.ScoreTier1Confidence(raw, openAIChatFieldSpec(meta.Direction))
		}
		if p.DetectedSpec == "" {
			p.DetectedSpec = "openai-chat"
		}
	}
	return p, err
}

// openAIChatFieldSpec returns the declared top-level wire keys for the
// OpenAI Chat Completions surface in direction d.
//
// Required keys reflect what a complete parse needs to surface usable
// canonical fields; absence drives the parse below the Coordinator's
// 0.70 threshold and triggers Tier-2 fallback. Optional keys are
// recognised but their absence is cosmetic.
func openAIChatFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "messages"},
			Optional: []string{
				"stream", "tools", "temperature", "top_p", "max_tokens",
				"max_completion_tokens", "stop", "frequency_penalty",
				"presence_penalty", "n", "seed", "logprobs", "top_logprobs",
				"logit_bias", "response_format", "user", "stream_options",
				"parallel_tool_calls", "tool_choice", "reasoning_effort",
				"modalities", "audio", "service_tier", "metadata", "store",
			},
		}
	}
	return core.FieldSpec{
		Required: []string{"model", "choices", "usage"},
		Optional: []string{
			"id", "object", "created", "system_fingerprint", "service_tier",
			"prompt_filter_results", "x_groq", "x_request_id",
		},
	}
}

type openAIChatRequest struct {
	Model       string                     `json:"model"`
	Messages    []openAIChatMessage        `json:"messages"`
	Tools       []openAIToolDef            `json:"tools,omitempty"`
	Stream      bool                       `json:"stream,omitempty"`
	Temperature *float64                   `json:"temperature,omitempty"`
	TopP        *float64                   `json:"top_p,omitempty"`
	MaxTokens   *int                       `json:"max_tokens,omitempty"`
	Stop        json.RawMessage            `json:"stop,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// ReasoningContent carries chain-of-thought / reasoning text on
	// reasoning-capable providers that use the OpenAI Chat wire shape
	// (DeepSeek `reasoning_content`, Moonshot, etc.). Projected to
	// core.ContentReasoning blocks so audit readers see the model's thinking
	// alongside its visible output.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Reasoning is the alternate wire name some OpenAI-compatible
	// providers (xAI, OpenRouter) use for the same chain-of-thought
	// text as reasoning_content. Whichever field is non-empty wins.
	Reasoning string `json:"reasoning,omitempty"`
}

// firstNonEmptyString returns the first non-empty argument; used to
// collapse the reasoning_content / reasoning wire-name aliases into one
// canonical reasoning text source.
func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type openAIToolCall struct {
	// Index is present on streaming delta tool_call objects only
	// (OpenAI spec: tool_calls[i].index). It identifies which tool call
	// in the aggregation map this delta belongs to. Absent on non-streaming
	// message.tool_calls[], where it is unused and ignored.
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

func (n *OpenAIChatNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req openAIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return core.NormalizedPayload{
			Kind:             core.KindAIChat,
			NormalizeVersion: core.SchemaVersion,
			Protocol:         "openai-chat",
		}, fmt.Errorf("openai-chat: request unmarshal: %w", err)
	}
	if len(req.Messages) == 0 {
		// /v1/completions sends `prompt` instead of `messages` — registry
		// routes that to a different normalizer; if it landed here, the
		// payload didn't match openai-chat.
		return core.NormalizedPayload{
			Kind:             core.KindAIChat,
			NormalizeVersion: core.SchemaVersion,
			Protocol:         "openai-chat",
			Model:            req.Model,
		}, fmt.Errorf("openai-chat: missing messages[]: %w", core.ErrUnsupported)
	}

	msgs := make([]core.Message, 0, len(req.Messages))
	for _, raw := range req.Messages {
		msg := core.Message{Role: roleFromString(raw.Role)}
		msg.Content = decodeOpenAIContent(raw.Content, raw.ToolCalls, raw.ToolCallID, firstNonEmptyString(raw.ReasoningContent, raw.Reasoning))
		msgs = append(msgs, msg)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-chat",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Stream:           req.Stream,
		Messages:         msgs,
	}

	if len(req.Tools) > 0 {
		tools := make([]core.ToolDef, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue
			}
			td := core.ToolDef{Name: t.Function.Name, Description: t.Function.Description}
			if len(t.Function.Parameters) > 0 {
				var p map[string]any
				if err := json.Unmarshal(t.Function.Parameters, &p); err == nil {
					td.ParametersJSONSchema = p
				}
			}
			tools = append(tools, td)
		}
		out.Tools = tools
	}

	if req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil || len(req.Stop) > 0 {
		params := &core.SamplingParam{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxTokens,
		}
		// stop may be string or []string in OpenAI's API.
		if len(req.Stop) > 0 {
			var s string
			if err := json.Unmarshal(req.Stop, &s); err == nil && s != "" {
				params.Stop = []string{s}
			} else {
				var ss []string
				if err := json.Unmarshal(req.Stop, &ss); err == nil {
					params.Stop = ss
				}
			}
		}
		out.Params = params
	}

	return out, nil
}

// decodeOpenAIContent unpacks OpenAI's polymorphic `content` field
// (string OR array of content parts) plus the parallel tool_calls /
// tool_call_id metadata that some roles carry. Reasoning text (from
// providers that ship `reasoning_content` on the same message) is
// prepended as a core.ContentReasoning block so audit readers see the
// chain-of-thought even when the visible `content` is empty (a common
// shape on `finish_reason=length` mid-reasoning truncations).
func decodeOpenAIContent(rawContent json.RawMessage, toolCalls []openAIToolCall, toolCallID, reasoning string) []core.ContentBlock {
	var blocks []core.ContentBlock

	if reasoning != "" {
		blocks = append(blocks, core.ContentBlock{Type: core.ContentReasoning, Text: reasoning})
	}

	if len(rawContent) > 0 && string(rawContent) != "null" {
		// Try string first.
		var asString string
		if err := json.Unmarshal(rawContent, &asString); err == nil {
			if asString != "" {
				if toolCallID != "" {
					// role=tool with a string content is a tool result.
					blocks = append(blocks, core.ContentBlock{
						Type: core.ContentToolResult,
						ToolResult: &core.ToolResult{
							CallID: toolCallID,
							Output: asString,
						},
					})
				} else {
					blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: asString})
				}
			}
		} else {
			// Array of content parts.
			var parts []map[string]any
			if err := json.Unmarshal(rawContent, &parts); err == nil {
				for _, part := range parts {
					blocks = append(blocks, openAIContentPart(part))
				}
			}
		}
	}

	for _, tc := range toolCalls {
		if tc.Type != "function" {
			continue
		}
		var input map[string]any
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		}
		blocks = append(blocks, core.ContentBlock{
			Type: core.ContentToolUse,
			ToolUse: &core.ToolUse{
				CallID: tc.ID,
				Name:   tc.Function.Name,
				Input:  input,
			},
		})
	}
	return blocks
}

func openAIContentPart(part map[string]any) core.ContentBlock {
	t, _ := part["type"].(string)
	switch t {
	case "text":
		s, _ := part["text"].(string)
		return core.ContentBlock{Type: core.ContentText, Text: s}
	case "image_url":
		if iu, ok := part["image_url"].(map[string]any); ok {
			urlStr, _ := iu["url"].(string)
			return core.ContentBlock{
				Type:     core.ContentImageRef,
				ImageRef: &core.BinaryRef{ContentType: "image", SpillKey: urlStr},
			}
		}
		return core.ContentBlock{Type: core.ContentImageRef, ImageRef: &core.BinaryRef{ContentType: "image"}}
	default:
		// Preserve as text serialization of the unknown part so audit
		// readers can see what the upstream sent.
		b, _ := json.Marshal(part)
		return core.ContentBlock{Type: core.ContentText, Text: string(b)}
	}
}

type openAIChatResponse struct {
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   *openAIUsage       `json:"usage,omitempty"`
}

type openAIChatChoice struct {
	Index        int                `json:"index"`
	FinishReason string             `json:"finish_reason"`
	Message      *openAIChatMessage `json:"message,omitempty"`
	Delta        *openAIChatMessage `json:"delta,omitempty"`
}

// openAIUsage captures every alias variant of the OpenAI-shaped usage
// object observed across the OpenAI-compatible provider ecosystem.
// First-match-wins resolution happens in
// (openAIUsage).extractCanonicalUsage.
//
// Top-level alias chain (token counts):
//   - prompt_tokens / completion_tokens / total_tokens — OpenAI canonical.
//   - input_tokens / output_tokens — OpenAI Responses-shape fallback.
//
// Cache-read alias chain (read-side hit at the upstream's prefix cache):
//   - prompt_tokens_details.cached_tokens — OpenAI canonical (2024-09+).
//   - input_tokens_details.cached_tokens — OpenAI Responses API.
//   - prompt_cache_hit_tokens — DeepSeek.
//   - prompt_cache_tokens — Moonshot explicit-cache API.
//   - cached_tokens (flat) — Kimi K2 / K2.5 / K2.6 auto-prefix cache.
//
// Reasoning chain:
//   - completion_tokens_details.reasoning_tokens — OpenAI o-series /
//     DeepSeek-reasoner / Moonshot kimi-k2-thinking.
//   - output_tokens_details.reasoning_tokens — OpenAI Responses API.
//
// Cache-write surcharge:
//   - prompt_tokens_details.cache_creation_tokens — Nexus extension emitted
//     by spec_anthropic when the upstream reported cache_creation_input_tokens.
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// Responses-shape top-level aliases.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// Top-level cache-read aliases.
	FlatCachedTokens     int `json:"cached_tokens,omitempty"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheTokens    int `json:"prompt_cache_tokens,omitempty"`
	// Nested details — OpenAI canonical 2024-09+.
	PromptTokensDetails *struct {
		CachedTokens        int `json:"cached_tokens,omitempty"`
		CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	// Nested details — OpenAI Responses API.
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens,omitempty"`
	} `json:"input_tokens_details,omitempty"`
	// Reasoning detail blocks.
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	} `json:"completion_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	} `json:"output_tokens_details,omitempty"`
}

// extractCanonicalUsage builds a canonical Usage from openAIUsage,
// applying first-match-wins resolution across the alias chains. Returns
// nil when no field was reported at all.
func (u openAIUsage) extractCanonicalUsage() *core.Usage {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.FlatCachedTokens == 0 && u.PromptCacheHitTokens == 0 && u.PromptCacheTokens == 0 &&
		u.PromptTokensDetails == nil && u.InputTokensDetails == nil &&
		u.CompletionTokensDetails == nil && u.OutputTokensDetails == nil {
		return nil
	}
	out := &core.Usage{}
	// Prompt tokens with Responses-shape fallback.
	switch {
	case u.PromptTokens != 0:
		setIntPtr(&out.PromptTokens, u.PromptTokens)
	case u.InputTokens != 0:
		setIntPtr(&out.PromptTokens, u.InputTokens)
	}
	// Completion tokens with Responses-shape fallback.
	switch {
	case u.CompletionTokens != 0:
		setIntPtr(&out.CompletionTokens, u.CompletionTokens)
	case u.OutputTokens != 0:
		setIntPtr(&out.CompletionTokens, u.OutputTokens)
	}
	if u.TotalTokens != 0 {
		setIntPtr(&out.TotalTokens, u.TotalTokens)
	}
	// Cache-read alias chain.
	switch {
	case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != 0:
		setIntPtr(&out.CacheReadTokens, u.PromptTokensDetails.CachedTokens)
	case u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != 0:
		setIntPtr(&out.CacheReadTokens, u.InputTokensDetails.CachedTokens)
	case u.PromptCacheHitTokens != 0:
		setIntPtr(&out.CacheReadTokens, u.PromptCacheHitTokens)
	case u.PromptCacheTokens != 0:
		setIntPtr(&out.CacheReadTokens, u.PromptCacheTokens)
	case u.FlatCachedTokens != 0:
		setIntPtr(&out.CacheReadTokens, u.FlatCachedTokens)
	}
	// Cache-write surcharge (Nexus extension; only Anthropic populates this path today).
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CacheCreationTokens != 0 {
		setIntPtr(&out.CacheCreationTokens, u.PromptTokensDetails.CacheCreationTokens)
	}
	// Reasoning chain.
	switch {
	case u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens != 0:
		setIntPtr(&out.ReasoningTokens, u.CompletionTokensDetails.ReasoningTokens)
	case u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens != 0:
		setIntPtr(&out.ReasoningTokens, u.OutputTokensDetails.ReasoningTokens)
	}
	return out
}

func (n *OpenAIChatNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	// cp / agent captures often lose the Content-Type on the response
	// side. Sniff the first bytes for the SSE `data:` prefix so SSE
	// bodies get the stream decoder even when meta.Stream is false.
	if meta.Stream || looksLikeOpenAIEventStream(raw) {
		return n.normalizeStreamResponse(raw, meta)
	}
	return n.normalizeNonStreamResponse(raw, meta)
}

// looksLikeOpenAIEventStream returns true when the captured bytes look
// like an OpenAI-Chat SSE stream (begins with `data:`). Used as a
// fallback when Content-Type wasn't preserved on the audit envelope.
func looksLikeOpenAIEventStream(raw []byte) bool {
	probe := raw
	if len(probe) > 64 {
		probe = probe[:64]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	return strings.HasPrefix(s, "data:")
}

func (n *OpenAIChatNormalizer) normalizeNonStreamResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp openAIChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return core.NormalizedPayload{
			Kind:             core.KindAIChat,
			NormalizeVersion: core.SchemaVersion,
			Protocol:         "openai-chat",
		}, fmt.Errorf("openai-chat: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-chat",
		Model:            firstNonEmpty(resp.Model, meta.Model),
	}
	// Extract Usage FIRST so callers that only have a usage-only body
	// (e.g., providers.ExtractUsage shim sees an upstream-final chunk
	// or a usage-only diagnostic response) still get the canonical
	// Usage even when choices[] is missing. The choices check below
	// only affects the Messages / FinishReason fields.
	if resp.Usage != nil {
		out.Usage = resp.Usage.extractCanonicalUsage()
	}
	if len(resp.Choices) == 0 {
		return out, fmt.Errorf("openai-chat: no choices in response: %w", core.ErrUnsupported)
	}
	out.Messages = make([]core.Message, 0, len(resp.Choices))
	reasoningChars := 0
	for _, ch := range resp.Choices {
		if ch.Message == nil {
			continue
		}
		reasoningChars += len(firstNonEmptyString(ch.Message.ReasoningContent, ch.Message.Reasoning))
		msg := core.Message{
			Role:         roleFromString(ch.Message.Role),
			Content:      decodeOpenAIContent(ch.Message.Content, ch.Message.ToolCalls, ch.Message.ToolCallID, firstNonEmptyString(ch.Message.ReasoningContent, ch.Message.Reasoning)),
			FinishReason: ch.FinishReason,
		}
		out.Messages = append(out.Messages, msg)
	}
	if len(resp.Choices) > 0 {
		out.FinishReason = resp.Choices[0].FinishReason
	}
	// Moonshot (kimi-k2.x) ships reasoning as `message.reasoning_content`
	// but does NOT populate `usage.completion_tokens_details.reasoning_
	// tokens` — so extractCanonicalUsage leaves ReasoningTokens nil even
	// though the model clearly reasoned. Derive a heuristic count from
	// the reasoning_content text length (chars/3.5, matching the
	// estimator's default OpenAI-family tokenizer) so dashboards +
	// reasoning_ratio widgets see a non-zero value. We only derive when
	// the wire didn't already provide an explicit count (the explicit
	// number from providers that DO report it takes precedence).
	if reasoningChars > 0 {
		if out.Usage == nil {
			out.Usage = &core.Usage{}
		}
		if out.Usage.ReasoningTokens == nil {
			est := reasoningChars * 2 / 7 // chars / 3.5, integer-safe
			if est < 1 {
				est = 1
			}
			out.Usage.ReasoningTokens = &est
		}
	}
	return out, nil
}

func zeroPayloadForKind(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-chat",
		Model:            meta.Model,
	}
}

func roleFromString(s string) core.Role {
	switch strings.ToLower(s) {
	case "system":
		return core.RoleSystem
	case "user":
		return core.RoleUser
	case "assistant":
		return core.RoleAssistant
	case "tool", "function":
		return core.RoleTool
	default:
		return core.Role(s)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func setIntPtr(dst **int, v int) {
	if v == 0 {
		return
	}
	x := v
	*dst = &x
}
