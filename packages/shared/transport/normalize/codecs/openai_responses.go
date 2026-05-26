// openai_responses.go — Tier-1 normalizer for OpenAI's Responses API
// (`POST /v1/responses`), wire-compatible with any provider that natively
// serves the Responses-API shape (currently OpenAI direct only, but the
// shape is intentionally open for adapters to adopt).
//
// Why a distinct normalizer from openai_chat? The two endpoints share a
// vendor + auth surface but the body schemas are different enough that
// trying to fit Responses-API into the Chat parser would either lose
// fidelity (reasoning summaries, output[] item types, status, etc.) or
// produce ambiguous DetectedSpec stamps. Keeping them as parallel
// normalizers — registered under distinct path keys
// (`openai::/v1/chat/completions` vs `openai::/v1/responses`) — keeps
// each parser focused.
//
// Both request and response sides are covered:
//
//   - Request:  `{input: [{role:"user", content:[{type:"input_text", text:"..."}|{type:"input_image", ...}]}], instructions, model, ...}`
//     `instructions` is the system-prompt equivalent (free string).
//     `input[]` items each have role + a content[] array of typed
//     parts. The role-on-input-item pattern matches Anthropic's content
//     blocks more than OpenAI Chat's `role`+`content` pair, so the
//     dispatch is purely path-keyed — never body-shape-keyed.
//
//   - Response: `{id, model, object:"response", status, output:[...], usage:{input_tokens, output_tokens, total_tokens}}`
//     `output[]` items have type ∈ {"reasoning","message","function_call","tool_call",...}.
//     "reasoning" items have `summary:[{type:"summary_text", text:"..."}]`
//     "message" items have `content:[{type:"output_text", text:"..."}]`
//     We project reasoning → core.ContentReasoning, output_text → core.ContentText,
//     function_call → core.ContentToolUse so the projected core.NormalizedPayload
//     is structurally identical to what the Chat normalizer produces
//     (downstream hooks + analytics see one canonical shape).
//
// Usage token normalisation: Responses-API uses `input_tokens` /
// `output_tokens` instead of `prompt_tokens` / `completion_tokens`. We
// map to the canonical PromptTokens / CompletionTokens / TotalTokens
// fields so cost math + dashboards key-stably across both ingresses.

package codecs

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAIResponsesNormalizer handles OpenAI's `/v1/responses` surface
// (request + non-streaming response). For streaming, the SSE event
// stream is decoded upstream by spec_openai/codec_responses.go's
// StreamDecoder and replayed as canonical chat-completion frames at
// the audit-emit boundary; this normalizer sees only the
// already-canonical bytes in that path.
type OpenAIResponsesNormalizer struct{}

// NewOpenAIResponsesNormalizer returns a normalizer instance. Stateless
// — safe to share across goroutines.
func NewOpenAIResponsesNormalizer() *OpenAIResponsesNormalizer {
	return &OpenAIResponsesNormalizer{}
}

func (n *OpenAIResponsesNormalizer) ID() string { return "openai-responses" }

func (n *OpenAIResponsesNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroPayloadForKind(meta), fmt.Errorf("openai-responses: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroPayloadForKind(meta), fmt.Errorf("openai-responses: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, openAIResponsesFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "openai-responses"
		}
	}
	return p, err
}

// openAIResponsesFieldSpec returns the declared top-level wire keys for
// OpenAI's /v1/responses surface in direction d.
//
// The Responses API uses `input` (not `messages`), `instructions` (not
// a system message), `output[]` (not `choices`), and `input_tokens` /
// `output_tokens` (not `prompt_tokens` / `completion_tokens`) — a
// different field set than Chat Completions, which is why this surface
// has its own normalizer + FieldSpec.
func openAIResponsesFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "input"},
			Optional: []string{
				"instructions", "max_output_tokens", "temperature", "top_p",
				"tools", "stream", "previous_response_id", "reasoning",
				"include", "metadata", "parallel_tool_calls", "service_tier",
				"truncation", "store", "tool_choice", "modalities",
			},
		}
	}
	return core.FieldSpec{
		Required: []string{"model", "output", "usage", "status"},
		Optional: []string{
			"id", "object", "created_at", "instructions", "metadata",
			"previous_response_id", "tools", "incomplete_details",
			"max_output_tokens", "temperature", "top_p", "parallel_tool_calls",
			"reasoning", "service_tier", "store", "truncation", "user",
			"error", "tool_choice",
		},
	}
}

// Request side

type openaiResponsesRequest struct {
	Model        string                     `json:"model,omitempty"`
	Instructions string                     `json:"instructions,omitempty"`
	// Per OpenAI Responses-API spec, `input` is polymorphic: either a
	// string shorthand ("hello") that maps to a single user message,
	// or an array of input items with role+content blocks. We decode as
	// RawMessage and dispatch on the leading byte in normalizeRequest.
	// Treating it as []openaiResponsesInputItem unconditionally fails
	// with `json: cannot unmarshal string into Go struct field ...input`
	// on every SDK using the string shorthand (anthropic-sdk, openai-py
	// 1.x default examples).
	Input        json.RawMessage           `json:"input,omitempty"`
	Tools        []openaiResponsesToolDecl `json:"tools,omitempty"`
	Temperature  *float64                  `json:"temperature,omitempty"`
	TopP         *float64                  `json:"top_p,omitempty"`
	MaxOutTokens *int                      `json:"max_output_tokens,omitempty"`
}

type openaiResponsesInputItem struct {
	Role    string                          `json:"role,omitempty"`
	Content []openaiResponsesInputContent   `json:"content,omitempty"`
	// Some clients emit `input` items WITHOUT role (raw string parts);
	// we treat those as user-role for safety.
	Type string `json:"type,omitempty"`
}

type openaiResponsesInputContent struct {
	Type     string `json:"type,omitempty"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	// Future: input_audio, input_file, etc.
}

type openaiResponsesToolDecl struct {
	Type        string         `json:"type,omitempty"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func (n *OpenAIResponsesNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req openaiResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroPayloadForKind(meta), fmt.Errorf("openai-responses: request unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-responses",
		Model:            firstNonEmpty(req.Model, meta.Model),
	}
	// `instructions` is the Responses-API equivalent of OpenAI Chat's
	// system message. Surface it as a system-role message at the head
	// of the conversation so audit + hook consumers see a uniform
	// shape across both ingresses.
	if strings.TrimSpace(req.Instructions) != "" {
		out.Messages = append(out.Messages, core.Message{
			Role:    core.RoleSystem,
			Content: []core.ContentBlock{{Type: core.ContentText, Text: req.Instructions}},
		})
	}
	// Dispatch on the polymorphic `input` shape (see openaiResponsesRequest
	// field comment): string shorthand → single user message, array → per-
	// item decode.
	if items, ok := decodeOpenAIResponsesInput(req.Input); ok {
		for _, item := range items {
			role := roleFromString(item.Role)
			if role == "" {
				// Default to user when role is omitted (some SDKs send bare
				// input items as user content).
				role = core.RoleUser
			}
			blocks := openaiResponsesInputContentToBlocks(item.Content)
			if len(blocks) == 0 {
				// Skip empty input items rather than producing zero-content
				// messages that downstream hooks then have to filter.
				continue
			}
			out.Messages = append(out.Messages, core.Message{Role: role, Content: blocks})
		}
	}
	for _, t := range req.Tools {
		// Responses-API tool decl is flat (Chat wraps under
		// `function:{...}`). Normalise to the same ToolDef shape so
		// hooks see one canonical schema.
		if t.Name == "" {
			continue
		}
		out.Tools = append(out.Tools, core.ToolDef{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJSONSchema: t.Parameters,
		})
	}
	if req.Temperature != nil || req.TopP != nil || req.MaxOutTokens != nil {
		out.Params = &core.SamplingParam{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxOutTokens,
		}
	}
	return out, nil
}

// decodeOpenAIResponsesInput accepts the polymorphic `input` field —
// either a JSON string ("hello") or a JSON array of input items — and
// returns a uniform []openaiResponsesInputItem. ok=false means the raw
// bytes were empty / null / unrecognised shape; callers treat that as
// "no input items" (an instructions-only request, for example, is
// legal). String shorthand becomes a single user-role item with one
// input_text content block, matching how the gateway codec
// (packages/ai-gateway/internal/providers/specs/openai/responses/codec_responses.go:343)
// expands it into the canonical message list.
func decodeOpenAIResponsesInput(raw json.RawMessage) ([]openaiResponsesInputItem, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, false
		}
		return []openaiResponsesInputItem{{
			Role:    "user",
			Content: []openaiResponsesInputContent{{Type: "input_text", Text: s}},
		}}, true
	case '[':
		var items []openaiResponsesInputItem
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, false
		}
		return items, true
	}
	return nil, false
}

func openaiResponsesInputContentToBlocks(parts []openaiResponsesInputContent) []core.ContentBlock {
	if len(parts) == 0 {
		return nil
	}
	out := make([]core.ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "":
			if p.Text != "" {
				out = append(out, core.ContentBlock{Type: core.ContentText, Text: p.Text})
			}
		case "input_image":
			// We could populate an ImageRef block with a stableHashHint
			// of the URL, but the Responses-API image input is usually a
			// pre-uploaded reference rather than inline base64; leave as
			// a typed text marker for now so downstream code knows an
			// image was present without us pretending to hash it.
			out = append(out, core.ContentBlock{Type: core.ContentImageRef, ImageRef: &core.BinaryRef{}})
		default:
			// Unknown part type — keep its text payload if any.
			if p.Text != "" {
				out = append(out, core.ContentBlock{Type: core.ContentText, Text: p.Text})
			}
		}
	}
	return out
}

// Response side

type openaiResponsesResponse struct {
	ID     string                       `json:"id,omitempty"`
	Object string                       `json:"object,omitempty"`
	Model  string                       `json:"model,omitempty"`
	Status string                       `json:"status,omitempty"`
	Output []openaiResponsesOutputItem  `json:"output,omitempty"`
	Usage  *openaiResponsesUsage        `json:"usage,omitempty"`
}

type openaiResponsesOutputItem struct {
	Type    string                        `json:"type,omitempty"`
	ID      string                        `json:"id,omitempty"`
	Role    string                        `json:"role,omitempty"`
	Status  string                        `json:"status,omitempty"`
	Summary []openaiResponsesSummaryPart  `json:"summary,omitempty"` // for type=reasoning
	Content []openaiResponsesOutputPart   `json:"content,omitempty"` // for type=message
	// Function/tool call fields.
	Name      string         `json:"name,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Arguments string         `json:"arguments,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
}

type openaiResponsesSummaryPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type openaiResponsesOutputPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type openaiResponsesUsage struct {
	InputTokens        int                            `json:"input_tokens,omitempty"`
	OutputTokens       int                            `json:"output_tokens,omitempty"`
	TotalTokens        int                            `json:"total_tokens,omitempty"`
	InputTokensDetails *openaiResponsesInputDetails   `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *openaiResponsesOutputDetails `json:"output_tokens_details,omitempty"`
}

type openaiResponsesInputDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type openaiResponsesOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

func (n *OpenAIResponsesNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp openaiResponsesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroPayloadForKind(meta), fmt.Errorf("openai-responses: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-responses",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		FinishReason:     resp.Status,
	}

	// The Responses-API surfaces assistant output as an array of typed
	// items — reasoning summaries arrive as their own item alongside
	// the message item. Collect all the text/reasoning blocks from
	// every item into a SINGLE assistant Message so downstream
	// consumers see the same canonical shape as Chat Completions
	// (one assistant message per response with mixed content blocks).
	var blocks []core.ContentBlock
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			for _, s := range item.Summary {
				// All summary part types we've seen carry the
				// reasoning trace in `text`. Be permissive about the
				// type label — providers add new ones over time.
				if s.Text != "" {
					blocks = append(blocks, core.ContentBlock{Type: core.ContentReasoning, Text: s.Text})
				}
			}
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" || c.Type == "" {
					if c.Text != "" {
						blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: c.Text})
					}
				}
			}
		case "function_call", "tool_call":
			// Tool-call output items. Project to core.ContentToolUse so the
			// canonical projector (and any hook that walks tool_use)
			// sees the same shape as Chat Completions.
			input := item.Input
			if len(input) == 0 && item.Arguments != "" {
				// Some clients emit a JSON-encoded arguments string
				// alongside (or instead of) the parsed input map.
				_ = json.Unmarshal([]byte(item.Arguments), &input)
			}
			tu := &core.ToolUse{
				CallID: firstNonEmpty(item.CallID, item.ID),
				Name:   item.Name,
				Input:  input,
			}
			blocks = append(blocks, core.ContentBlock{Type: core.ContentToolUse, ToolUse: tu})
		}
	}

	if len(blocks) == 0 {
		// Defensive: response with no output_text + no reasoning still
		// gets an assistant message so downstream code that filters by
		// Role doesn't drop the row entirely.
		blocks = []core.ContentBlock{{Type: core.ContentText, Text: ""}}
	}
	out.Messages = []core.Message{{
		Role:         core.RoleAssistant,
		Content:      blocks,
		FinishReason: resp.Status,
	}}

	if resp.Usage != nil {
		u := &core.Usage{}
		setIntPtr(&u.PromptTokens, resp.Usage.InputTokens)
		setIntPtr(&u.CompletionTokens, resp.Usage.OutputTokens)
		setIntPtr(&u.TotalTokens, resp.Usage.TotalTokens)
		if resp.Usage.InputTokensDetails != nil && resp.Usage.InputTokensDetails.CachedTokens > 0 {
			v := resp.Usage.InputTokensDetails.CachedTokens
			u.CacheReadTokens = &v
		}
		if resp.Usage.OutputTokensDetails != nil && resp.Usage.OutputTokensDetails.ReasoningTokens > 0 {
			v := resp.Usage.OutputTokensDetails.ReasoningTokens
			u.ReasoningTokens = &v
		}
		out.Usage = u
	}
	return out, nil
}
