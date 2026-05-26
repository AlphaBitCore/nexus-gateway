package codecs

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// Normalize routes by direction.
func (n *AnthropicMessagesNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: empty body: %w", core.ErrUnsupported)
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
	// Stamp Tier-1 Confidence using the anthropic-messages FieldSpec —
	// see confidence.go. Anthropic responses carry their own field set
	// (content/stop_reason/usage at the response root, NOT choices); the
	// declared specs below let core.ScoreTier1Confidence detect spec drift
	// without false-positive penalising clean parses.
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, anthropicMessagesFieldSpec(meta.Direction))
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
	if meta.Stream || looksLikeAnthropicEventStream(raw) {
		// cp / agent captures often arrive without a content-type set
		// even when the body is SSE. Sniff the first bytes for the
		// canonical Anthropic event-stream prefix so we still get a
		// real assistant message instead of a JSON-unmarshal failure.
		return n.normalizeStreamResponse(raw, meta)
	}
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
		uncached := resp.Usage.InputTokens
		cacheRead := resp.Usage.CacheReadInputTokens
		cacheWrite := resp.Usage.CacheCreationInputTokens
		output := resp.Usage.OutputTokens

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
		// Anthropic's API counts thinking tokens as part of output_tokens
		// but never breaks down how many of those are thinking vs visible
		// text. Derive a heuristic by summing the character length of
		// every ContentReasoning (thinking) block × chars/3.5 (matches
		// the estimator's default Anthropic-family tokenizer). The
		// resulting count is approximate (±15%) but lets dashboards +
		// the reasoning_ratio widget surface a non-zero signal instead
		// of misclassifying every Claude row as "no reasoning happened".
		reasoningChars := 0
		for _, b := range blocks {
			if b.Type == core.ContentReasoning {
				reasoningChars += len(b.Text)
			}
		}
		if reasoningChars > 0 {
			est := reasoningChars * 2 / 7
			if est < 1 {
				est = 1
			}
			u.ReasoningTokens = &est
		}
		out.Usage = u
	}
	return out, nil
}

// Streaming response (event stream)

// streamBlockState accumulates one content block as deltas arrive.
type streamBlockState struct {
	blockType string
	text      strings.Builder
	tool      *core.ToolUse
	toolJSON  strings.Builder
}

func (n *AnthropicMessagesNormalizer) normalizeStreamResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "anthropic-messages",
		Model:            meta.Model,
		Stream:           true,
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var (
		eventName    string
		blocks       = map[int]*streamBlockState{}
		order        []int
		usage        *core.Usage
		finishReason string
		sawAny       bool
		lastErr      error
	)

	emit := func(eventName, dataLine string) {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(dataLine), &chunk); err != nil {
			lastErr = err
			return
		}
		switch eventName {
		case "message_start":
			if msg, ok := chunk["message"].(map[string]any); ok {
				if m, ok := msg["model"].(string); ok && out.Model == "" {
					out.Model = m
				}
				if u, ok := msg["usage"].(map[string]any); ok {
					usage = mergeAnthropicUsage(usage, u)
				}
			}
		case "content_block_start":
			idx := intFromAny(chunk["index"])
			cb, _ := chunk["content_block"].(map[string]any)
			st := &streamBlockState{}
			if cb != nil {
				st.blockType, _ = cb["type"].(string)
				if st.blockType == "tool_use" {
					tu := &core.ToolUse{}
					tu.CallID, _ = cb["id"].(string)
					tu.Name, _ = cb["name"].(string)
					st.tool = tu
				}
			}
			blocks[idx] = st
			order = append(order, idx)
		case "content_block_delta":
			idx := intFromAny(chunk["index"])
			st, ok := blocks[idx]
			if !ok {
				st = &streamBlockState{}
				blocks[idx] = st
				order = append(order, idx)
			}
			d, _ := chunk["delta"].(map[string]any)
			if d == nil {
				return
			}
			dtype, _ := d["type"].(string)
			switch dtype {
			case "text_delta":
				if s, ok := d["text"].(string); ok {
					st.text.WriteString(s)
					sawAny = true
				}
			case "thinking_delta":
				if s, ok := d["thinking"].(string); ok {
					st.text.WriteString(s)
					st.blockType = "thinking"
					sawAny = true
				} else if s, ok := d["text"].(string); ok {
					st.text.WriteString(s)
					st.blockType = "thinking"
					sawAny = true
				}
			case "input_json_delta":
				if s, ok := d["partial_json"].(string); ok {
					st.toolJSON.WriteString(s)
					sawAny = true
				}
			}
		case "content_block_stop":
			// nothing to do; stream may include text after stop on tools.
		case "message_delta":
			if d, ok := chunk["delta"].(map[string]any); ok {
				if r, _ := d["stop_reason"].(string); r != "" {
					finishReason = r
				}
			}
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = mergeAnthropicUsage(usage, u)
			}
		case "message_stop":
			// terminal event; nothing to assemble.
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			emit(eventName, data)
		}
	}
	if err := scanner.Err(); err != nil {
		lastErr = err
	}

	if !sawAny && finishReason == "" {
		return out, fmt.Errorf("anthropic-messages: no events decoded: %w", core.ErrUnsupported)
	}

	// Stitch the assembled blocks into a single assistant message.
	msg := core.Message{Role: core.RoleAssistant, FinishReason: finishReason}
	for _, idx := range order {
		st := blocks[idx]
		if st == nil {
			continue
		}
		switch st.blockType {
		case "thinking":
			if t := st.text.String(); t != "" {
				msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentReasoning, Text: t})
			}
		case "tool_use":
			tu := st.tool
			if tu == nil {
				tu = &core.ToolUse{}
			}
			if js := st.toolJSON.String(); js != "" {
				var input map[string]any
				if err := json.Unmarshal([]byte(js), &input); err == nil {
					tu.Input = input
				}
			}
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentToolUse, ToolUse: tu})
		default:
			if t := st.text.String(); t != "" {
				msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentText, Text: t})
			}
		}
	}
	out.Messages = []core.Message{msg}
	out.FinishReason = finishReason
	if usage != nil {
		out.Usage = usage
	}
	// Stream variant: same thinking-tokens heuristic as the non-stream
	// path (see normalizeResponse). Sum thinking block char-length and
	// surface as Usage.ReasoningTokens when the wire didn't already
	// provide an explicit count.
	reasoningChars := 0
	for _, b := range msg.Content {
		if b.Type == core.ContentReasoning {
			reasoningChars += len(b.Text)
		}
	}
	if reasoningChars > 0 {
		if out.Usage == nil {
			out.Usage = &core.Usage{}
		}
		if out.Usage.ReasoningTokens == nil {
			est := reasoningChars * 2 / 7
			if est < 1 {
				est = 1
			}
			out.Usage.ReasoningTokens = &est
		}
	}
	if lastErr != nil {
		return out, fmt.Errorf("anthropic-messages: stream parse incomplete: %w", lastErr)
	}
	return out, nil
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

// looksLikeAnthropicEventStream sniffs the first bytes to detect SSE
// without relying on the Content-Type header — useful when the producer
// (cp/agent) captures the body without preserving response headers.
func looksLikeAnthropicEventStream(raw []byte) bool {
	probe := raw
	if len(probe) > 64 {
		probe = probe[:64]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	return strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:")
}
