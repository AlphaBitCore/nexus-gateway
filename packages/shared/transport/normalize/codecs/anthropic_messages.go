package codecs

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// AnthropicMessagesNormalizer handles Anthropic's /v1/messages surface
// (request, non-streaming response, and the streamed event stream:
// message_start / content_block_start / content_block_delta /
// content_block_stop / message_delta / message_stop).
//
// Notable Anthropic-isms preserved in the canonical NormalizedPayload:
//
//   - `system` field is flattened into a synthetic system message at
//     position [0] in Messages, so downstream hooks see a uniform list.
//   - `thinking` content blocks (extended-thinking surface) survive as
//     core.ContentBlock{Type: core.ContentReasoning} rather than being
//     dropped — hooks can opt-in to scanning reasoning via TextProjectionWith.
//   - cache_creation_input_tokens / cache_read_input_tokens are mapped
//     onto Usage.CacheCreationTokens / CacheReadTokens.
type AnthropicMessagesNormalizer struct{}

// NewAnthropicMessagesNormalizer returns a stateless normalizer instance.
func NewAnthropicMessagesNormalizer() *AnthropicMessagesNormalizer {
	return &AnthropicMessagesNormalizer{}
}

// ID is the metric / log label.
func (n *AnthropicMessagesNormalizer) ID() string { return "anthropic-messages" }

// LooksLike implements core.Sniffer: reports whether raw opens like the
// Anthropic /v1/messages wire. Four shapes match, all probed within
// the leading bytes only:
//
//   - the SSE stream's first frame (`event: message_start` / a data
//     payload typed message_start) — the most distinctive AI framing
//     on any wire we capture;
//   - a non-stream response object carrying BOTH the Anthropic-only
//     `"type":"message"` discriminator and a `"stop_reason"` key
//     (Anthropic puts both near the object head; requiring the pair
//     keeps web-chat protocols that also type their frames "message"
//     from matching);
//   - a Bedrock-style request carrying `"anthropic_version"`;
//   - a request body carrying BOTH `"messages"` and `"max_tokens"` —
//     Anthropic requires max_tokens on every /v1/messages request, so
//     the pair is the tightest byte-level request discriminator the
//     wire offers. OpenAI Chat requests MAY also carry max_tokens
//     (shape-ambiguous); the sniff walk registers this codec before
//     openai-chat, so the stricter requirement wins first and the
//     request-direction keymissed goldens pin the discrimination.
//     Probed only when meta.Direction is request or unset: a response
//     body echoing those words must not divert the response probes.
//
// Precision over recall: a miss falls through to the Tier-2 pattern
// probe, but a false positive steals another protocol's traffic.
func (n *AnthropicMessagesNormalizer) LooksLike(raw []byte, meta core.Meta) bool {
	if LooksLikeAnthropicSSE(raw) {
		return true
	}
	probe := sniffProbe(raw)
	if bytes.Contains(probe, []byte(`"anthropic_version"`)) {
		return true
	}
	if meta.Direction != core.DirectionResponse &&
		bytes.Contains(probe, []byte(`"messages"`)) &&
		bytes.Contains(probe, []byte(`"max_tokens"`)) {
		return true
	}
	return bytes.Contains(probe, []byte(`"type":"message"`)) &&
		bytes.Contains(probe, []byte(`"stop_reason"`))
}

// Normalize routes by direction.
func (n *AnthropicMessagesNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: empty body: %w", core.ErrUnsupported)
	}
	// Streamed responses take the SSE fold, which stamps its own
	// coverage-based Confidence — an event stream has no top-level JSON
	// object for the FieldSpec scorer below to measure. The byte sniff
	// covers cp / agent captures that lost the stream flag and the
	// Content-Type header.
	if meta.Direction == core.DirectionResponse && (meta.Stream || LooksLikeAnthropicSSE(raw)) {
		return foldAnthropicSSE(raw, meta)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	// Confidence semantics (one meaning per input shape): a stream fold
	// computes frame coverage (recognized / total data frames) and sets
	// Confidence itself; single-document bodies score weighted field
	// coverage against the anthropic-messages FieldSpec — see
	// confidence.go. Anthropic responses carry their own field set
	// (content/stop_reason/usage at the response root, NOT choices); the
	// declared specs below let core.ScoreTier1Confidence detect spec drift
	// without false-positive penalising clean parses.
	if err == nil {
		if p.Confidence == 0 {
			p.Confidence = core.ScoreTier1Confidence(raw, anthropicMessagesFieldSpec(meta.Direction))
		}
		if p.DetectedSpec == "" {
			p.DetectedSpec = "anthropic-messages"
		}
	}
	return p, err
}

// anthropicMessagesFieldSpec returns the declared top-level wire keys
// for the Anthropic /v1/messages surface in direction d.
func anthropicMessagesFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "messages", "max_tokens"},
			Optional: []string{
				"system", "tools", "stream", "temperature", "top_p", "top_k",
				"stop_sequences", "metadata", "tool_choice", "thinking",
				"anthropic_version", "anthropic_beta",
			},
		}
	}
	return core.FieldSpec{
		Required: []string{"model", "content", "usage", "stop_reason"},
		Optional: []string{
			"id", "type", "role", "stop_sequence", "container",
		},
	}
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func (n *AnthropicMessagesNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: request unmarshal: %w", err)
	}
	if len(req.Messages) == 0 {
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: missing messages[]: %w", core.ErrUnsupported)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "anthropic-messages",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Stream:           req.Stream,
	}

	// Anthropic's `system` field may be a string or an array of content blocks.
	// Either way we project it to a synthetic system message[0].
	if len(req.System) > 0 && string(req.System) != "null" {
		blocks := anthropicSystemToBlocks(req.System)
		if len(blocks) > 0 {
			out.Messages = append(out.Messages, core.Message{Role: core.RoleSystem, Content: blocks})
		}
	}

	for _, m := range req.Messages {
		blocks := anthropicDecodeContent(m.Content)
		out.Messages = append(out.Messages, core.Message{Role: roleFromString(m.Role), Content: blocks})
	}

	if len(req.Tools) > 0 {
		tools := make([]core.ToolDef, 0, len(req.Tools))
		for _, t := range req.Tools {
			td := core.ToolDef{Name: t.Name, Description: t.Description}
			if len(t.InputSchema) > 0 {
				var p map[string]any
				if err := json.Unmarshal(t.InputSchema, &p); err == nil {
					td.ParametersJSONSchema = p
				}
			}
			tools = append(tools, td)
		}
		out.Tools = tools
	}

	if req.Temperature != nil || req.TopP != nil || req.TopK != nil || req.MaxTokens != nil || len(req.StopSequences) > 0 {
		out.Params = &core.SamplingParam{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			TopK:        req.TopK,
			MaxTokens:   req.MaxTokens,
			Stop:        req.StopSequences,
		}
	}

	return out, nil
}

// anthropicSystemToBlocks accepts either a plain string or a content-array
// for the `system` field and returns ContentBlocks suitable for the
// synthetic system message.
func anthropicSystemToBlocks(raw json.RawMessage) []core.ContentBlock {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []core.ContentBlock{{Type: core.ContentText, Text: s}}
	}
	return anthropicDecodeContent(raw)
}

// anthropicDecodeContent expands an Anthropic content field (string or
// content-block array) into ContentBlocks.
func anthropicDecodeContent(raw json.RawMessage) []core.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// String shortcut.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []core.ContentBlock{{Type: core.ContentText, Text: s}}
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Could not parse — keep raw text so audit readers see it.
		return []core.ContentBlock{{Type: core.ContentText, Text: string(raw)}}
	}
	out := make([]core.ContentBlock, 0, len(parts))
	for _, p := range parts {
		out = append(out, anthropicContentPart(p))
	}
	return out
}

func anthropicContentPart(part map[string]any) core.ContentBlock {
	t, _ := part["type"].(string)
	switch t {
	case "text":
		s, _ := part["text"].(string)
		return core.ContentBlock{Type: core.ContentText, Text: s}
	case "thinking":
		s, _ := part["thinking"].(string)
		if s == "" {
			// Some SDK shapes carry the reasoning text under "text".
			s, _ = part["text"].(string)
		}
		return core.ContentBlock{Type: core.ContentReasoning, Text: s}
	case "image":
		var ref core.BinaryRef
		ref.ContentType = "image"
		if src, ok := part["source"].(map[string]any); ok {
			if mt, _ := src["media_type"].(string); mt != "" {
				ref.ContentType = mt
			}
			if data, _ := src["data"].(string); data != "" {
				ref.SHA256 = stableHashHint(data)
				ref.Size = int64(len(data))
			}
		}
		return core.ContentBlock{Type: core.ContentImageRef, ImageRef: &ref}
	case "tool_use":
		tu := core.ToolUse{}
		tu.CallID, _ = part["id"].(string)
		tu.Name, _ = part["name"].(string)
		if in, ok := part["input"].(map[string]any); ok {
			tu.Input = in
		}
		return core.ContentBlock{Type: core.ContentToolUse, ToolUse: &tu}
	case "tool_result":
		tr := core.ToolResult{}
		tr.CallID, _ = part["tool_use_id"].(string)
		// Anthropic's tool_result.content may be string or content-block array.
		if s, ok := part["content"].(string); ok {
			tr.Output = s
		} else if arr, ok := part["content"].([]any); ok {
			var b strings.Builder
			for _, it := range arr {
				if m, ok := it.(map[string]any); ok {
					if txt, _ := m["text"].(string); txt != "" {
						b.WriteString(txt)
					}
				}
			}
			tr.Output = b.String()
		}
		return core.ContentBlock{Type: core.ContentToolResult, ToolResult: &tr}
	default:
		// Unknown — preserve as text serialization.
		b, _ := json.Marshal(part)
		return core.ContentBlock{Type: core.ContentText, Text: string(b)}
	}
}

// stableHashHint returns a short prefix of the inline base64 so the
// BinaryRef looks reasonable when the spill store wasn't engaged.
// Real audit pipelines populate the spill key via the spill backend;
// this hint is purely for inline-only test paths.
func stableHashHint(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

// Non-streaming response

type anthropicResponse struct {
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      *anthropicUsage `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

func (n *AnthropicMessagesNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "anthropic-messages",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		FinishReason:     resp.StopReason,
	}
	blocks := anthropicDecodeContent(resp.Content)
	out.Messages = []core.Message{{
		Role:         core.RoleAssistant,
		Content:      blocks,
		FinishReason: resp.StopReason,
	}}
	if resp.Usage != nil {
		// Anthropic's raw input_tokens is the UNCACHED count. The
		// canonical (OpenAI-style) PromptTokens is the TOTAL input =
		// uncached + cache_read + cache_creation. Stamping that
		// normalized value here keeps cost calculation uniform across
		// providers: UncachedInput = PromptTokens − CacheReadTokens −
		// CacheCreationTokens always yields the billable un-cached input
		// regardless of upstream convention.
		// Sum the character length of every ContentReasoning (thinking) block to
		// drive the reasoning-token estimate (see anthropicUsageToCanonical).
		reasoningChars := 0
		for _, b := range blocks {
			if b.Type == core.ContentReasoning {
				reasoningChars += len(b.Text)
			}
		}
		out.Usage = anthropicUsageToCanonical(resp.Usage, reasoningChars)
	}
	return out, nil
}

// anthropicUsageToCanonical projects an Anthropic usage block (plus the summed
// character length of the response's thinking blocks) into canonical Usage. It is
// the single source of the non-streaming Anthropic usage shape — called by the
// full normalizeResponse and by the usage-only fast path (ExtractUsageOnly), which
// pass the same reasoningChars so both yield identical Usage.
//
// Anthropic's raw input_tokens is the UNCACHED count; the canonical
// (OpenAI-style) PromptTokens is the TOTAL input = uncached + cache_read +
// cache_creation, so cost calculation stays uniform across providers
// (UncachedInput = PromptTokens − CacheReadTokens − CacheCreationTokens).
//
// Anthropic counts thinking tokens inside output_tokens but never breaks down how
// many are thinking vs visible text, so ReasoningTokens is a heuristic:
// reasoningChars × 2/7 (chars/3.5, the estimator's default Anthropic tokenizer).
// Approximate (±15%) but lets the reasoning_ratio widget show a non-zero signal
// instead of misclassifying every Claude row as "no reasoning". It does not affect
// the billed total — output_tokens already includes the thinking tokens.
func anthropicUsageToCanonical(raw *anthropicUsage, reasoningChars int) *core.Usage {
	uncached := raw.InputTokens
	cacheRead := raw.CacheReadInputTokens
	cacheWrite := raw.CacheCreationInputTokens
	output := raw.OutputTokens

	u := &core.Usage{}
	if uncached != 0 || cacheRead != 0 || cacheWrite != 0 {
		prompt := uncached + cacheRead + cacheWrite
		u.PromptTokens = &prompt
	}
	setIntPtr(&u.CompletionTokens, output)
	if cacheWrite != 0 {
		v := cacheWrite
		u.CacheCreationTokens = &v
	}
	if cacheRead != 0 {
		v := cacheRead
		u.CacheReadTokens = &v
	}
	// TotalTokens = full input + output (matches OpenAI convention).
	if u.PromptTokens != nil || output != 0 {
		tot := 0
		if u.PromptTokens != nil {
			tot += *u.PromptTokens
		}
		tot += output
		u.TotalTokens = &tot
	}
	if reasoningChars > 0 {
		est := reasoningChars * 2 / 7
		if est < 1 {
			est = 1
		}
		u.ReasoningTokens = &est
	}
	return u
}

// MergeAnthropicEventUsage is the EXPORTED variant of mergeAnthropicUsage
// used by ai-gateway's spec_anthropic streaming session.
// Accepts the raw JSON bytes of an Anthropic SSE event's data payload
// (e.g. `{"type":"message_start","message":{"usage":{...}}}` or
// `{"type":"message_delta","usage":{...}}`) and returns the running
// Usage updated with whatever fields the event surfaced. PromptTokens
// stays normalized to the OpenAI canonical convention (= uncached +
// cache_read + cache_creation).
//
// Returns prev unchanged when the event carries no usage fields.
// Returns prev with TotalTokens recomputed whenever any field changed.
func MergeAnthropicEventUsage(prev *core.Usage, eventDataJSON []byte) *core.Usage {
	var env map[string]any
	if err := json.Unmarshal(eventDataJSON, &env); err != nil || env == nil {
		return prev
	}
	// message_start nests usage under message.usage; message_delta has it at root.
	if msg, ok := env["message"].(map[string]any); ok {
		if u, ok := msg["usage"].(map[string]any); ok {
			return mergeAnthropicUsage(prev, u)
		}
	}
	if u, ok := env["usage"].(map[string]any); ok {
		return mergeAnthropicUsage(prev, u)
	}
	return prev
}

// mergeAnthropicUsage absorbs the Anthropic-shape usage map from a
// message_start or message_delta event into the running Usage state.
// PromptTokens carries the OpenAI-canonical TOTAL input (uncached +
// cache_read + cache_creation); see normalizeResponse for the rationale.
// Streaming events may emit usage incrementally; we recompute the
// normalized PromptTokens whenever any of the three input counters
// changes so the running snapshot is always consistent.
func mergeAnthropicUsage(prev *core.Usage, raw map[string]any) *core.Usage {
	if prev == nil {
		prev = &core.Usage{}
	}
	// Recover the previous uncached count from the normalized PromptTokens
	// (canonical PromptTokens = uncached + cache_read + cache_creation).
	prevCacheRead := derefIntPtr(prev.CacheReadTokens)
	prevCacheWrite := derefIntPtr(prev.CacheCreationTokens)
	prevPromptTotal := derefIntPtr(prev.PromptTokens)
	uncached := prevPromptTotal - prevCacheRead - prevCacheWrite
	if uncached < 0 {
		uncached = 0
	}
	cacheRead := prevCacheRead
	cacheWrite := prevCacheWrite
	output := derefIntPtr(prev.CompletionTokens)
	touched := false

	if v, ok := raw["input_tokens"]; ok {
		if i := intFromAny(v); i != 0 {
			uncached = i
			touched = true
		}
	}
	if v, ok := raw["cache_read_input_tokens"]; ok {
		if i := intFromAny(v); i != 0 {
			cacheRead = i
			touched = true
		}
	}
	if v, ok := raw["cache_creation_input_tokens"]; ok {
		if i := intFromAny(v); i != 0 {
			cacheWrite = i
			touched = true
		}
	}
	if v, ok := raw["output_tokens"]; ok {
		if i := intFromAny(v); i != 0 {
			output = i
			touched = true
		}
	}
	if !touched {
		return prev
	}

	if uncached+cacheRead+cacheWrite > 0 {
		prompt := uncached + cacheRead + cacheWrite
		prev.PromptTokens = &prompt
	}
	if cacheRead != 0 {
		v := cacheRead
		prev.CacheReadTokens = &v
	}
	if cacheWrite != 0 {
		v := cacheWrite
		prev.CacheCreationTokens = &v
	}
	if output != 0 {
		v := output
		prev.CompletionTokens = &v
	}
	if prev.PromptTokens != nil || prev.CompletionTokens != nil {
		tot := derefIntPtr(prev.PromptTokens) + derefIntPtr(prev.CompletionTokens)
		prev.TotalTokens = &tot
	}
	return prev
}

// derefIntPtr returns 0 when p is nil.
func derefIntPtr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

func zeroAnthropic(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "anthropic-messages",
		Model:            meta.Model,
	}
}
