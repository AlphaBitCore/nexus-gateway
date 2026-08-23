package codecs

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
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
	// Thinking is Anthropic's BUDGET spelling of the reasoning request.
	// `type` is "enabled" / "disabled"; a disabled block is the caller saying
	// NOT to reason, which is an expressed intent and is recorded as such.
	Thinking *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
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
	if err := decodeLenient(raw, &req); err != nil {
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

	for i, m := range req.Messages {
		blocks := anthropicDecodeContent(m.Content, locator.JoinPath("messages", i)+".content")
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

	if req.Temperature != nil || req.TopP != nil || req.TopK != nil || req.MaxTokens != nil ||
		len(req.StopSequences) > 0 || req.Thinking != nil {
		out.Params = &core.SamplingParam{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			TopK:        req.TopK,
			MaxTokens:   req.MaxTokens,
			Stop:        req.StopSequences,
		}
		// Recorded as the caller wrote it. A disabled block carries no budget,
		// so it lands as an Effort of "none" — the one word every wire's
		// vocabulary can express, and the only derivation done here, because
		// "do not reason" is not a quantity any codec can compute back from a
		// nil budget.
		if t := req.Thinking; t != nil {
			r := &core.Reasoning{BudgetTokens: t.BudgetTokens}
			if t.Type == "disabled" {
				r.Effort = "none"
				r.BudgetTokens = nil
			}
			if r.Asked() {
				out.Params.Reasoning = r
			}
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
	// The system field is text-only in practice; no positional context is
	// threaded because a media part there has no defined wire shape.
	return anthropicDecodeContent(raw, "")
}

// anthropicDecodeContent expands an Anthropic content field (string or
// content-block array) into ContentBlocks.
//
// base is the gjson path of the content array this call is expanding
// (e.g. "messages.0.content"); each part's locator is base + its index, so
// a media part addresses its own bytes inside the captured body. An empty
// base means the caller has no positional context — media parts then carry
// no locator and degrade to a fingerprint-free absent ref rather than
// pointing somewhere wrong.
func anthropicDecodeContent(raw json.RawMessage, base string) []core.ContentBlock {
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
		// A mixed-type array (a bare string beside a block object) fails the
		// typed unmarshal. Rendering the raw bytes here would copy any
		// payload inside it into scanned text.
		return []core.ContentBlock{{Type: core.ContentText, Text: payloadSafeRaw(raw)}}
	}
	out := make([]core.ContentBlock, 0, len(parts))
	for i, p := range parts {
		out = append(out, anthropicContentPart(p, locator.JoinPath(base, i))...)
	}
	return out
}

// anthropicContentPart returns the blocks one wire part projects to. It is
// a slice because a tool_result can carry media inside it: the result text
// and each nested image are separate blocks, and the media must not be
// flattened into the text the way it used to be.
func anthropicContentPart(part map[string]any, path string) []core.ContentBlock {
	return anthropicPartAtDepth(part, path, 0)
}

func anthropicPartAtDepth(part map[string]any, path string, depth int) []core.ContentBlock {
	t, _ := part["type"].(string)
	switch t {
	case "text":
		s, _ := part["text"].(string)
		return []core.ContentBlock{{Type: core.ContentText, Text: s}}
	case "thinking":
		s, _ := part["thinking"].(string)
		if s == "" {
			// Some SDK shapes carry the reasoning text under "text".
			s, _ = part["text"].(string)
		}
		return []core.ContentBlock{{Type: core.ContentReasoning, Text: s}}
	case "image", "document":
		// Images and documents share one `source` envelope, but two of its
		// five variants carry text rather than bytes. Routing those into a
		// media ref would drop the text — and because TextProjection feeds
		// the compliance hooks only from text-shaped blocks, dropping it
		// would hide the content from scanning entirely. Dispatch on the
		// source type first: bytes become media, prose stays prose.
		src, _ := part["source"].(map[string]any)
		switch st, _ := src["type"].(string); st {
		case "text":
			s, _ := src["data"].(string)
			return []core.ContentBlock{{Type: core.ContentText, Text: s}}
		case "content":
			// A nested block array — recurse so nested images keep their own
			// locators and nested text stays scannable.
			//
			// Recursion walks the ALREADY-PARSED value. Re-marshalling and
			// re-parsing at each level makes cost superlinear in body size,
			// and this decode runs synchronously on the request path: a
			// 120 KB deeply-nested body measured at 26 seconds that way.
			// Depth is also capped — nesting beyond a couple of levels is
			// not a shape any provider documents, and an unbounded walk on
			// caller-controlled input is a denial-of-service surface.
			return anthropicNestedBlocks(src["content"], locator.JoinSuffix(path, "source.content"), depth)
		default:
			modality := ""
			if t == "image" {
				// media_type is documented only on the base64 variant, so a
				// url/file-id image has no mime to infer from. Saying
				// "file" would render the wrong card and, worse, drop the
				// vision requirement in capability routing.
				modality = core.ModalityImage
			}
			return []core.ContentBlock{mediaBlock(anthropicSourceMedia(part, path, modality))}
		}
	case "container_upload":
		id, _ := part["file_id"].(string)
		return []core.ContentBlock{mediaBlock(providerRefMedia("", id, core.ModalityFile))}
	case "tool_use":
		tu := core.ToolUse{}
		tu.CallID, _ = part["id"].(string)
		tu.Name, _ = part["name"].(string)
		if in, ok := part["input"].(map[string]any); ok {
			tu.Input = in
		}
		return []core.ContentBlock{{Type: core.ContentToolUse, ToolUse: &tu}}
	case "tool_result":
		tr := core.ToolResult{}
		tr.CallID, _ = part["tool_use_id"].(string)
		var nested []core.ContentBlock
		// Anthropic's tool_result.content may be string or content-block array.
		if s, ok := part["content"].(string); ok {
			tr.Output = s
		} else if arr, ok := part["content"].([]any); ok {
			var b strings.Builder
			for i, it := range arr {
				m, ok := it.(map[string]any)
				if !ok {
					continue
				}
				// A tool that returns a screenshot puts an image block here.
				// Only text was ever read, so those images vanished.
				switch mt, _ := m["type"].(string); mt {
				case "image", "document":
					nested = append(nested, anthropicPartAtDepth(
						m, locator.JoinPath(locator.JoinSuffix(path, "content"), i), depth+1)...)
				default:
					if txt, _ := m["text"].(string); txt != "" {
						b.WriteString(txt)
					}
				}
			}
			tr.Output = b.String()
		}
		return append([]core.ContentBlock{{Type: core.ContentToolResult, ToolResult: &tr}}, nested...)
	default:
		// Unknown — preserve as text so the reader sees what arrived. An
		// unknown block may still carry a base64 source, so the rendering
		// elides payloads rather than trusting that it cannot.
		return []core.ContentBlock{{Type: core.ContentText, Text: payloadSafeJSON(part)}}
	}
}

// anthropicNestedBlocks projects an already-parsed nested content array.
// depth guards against caller-controlled nesting; past the cap the content
// is kept as text rather than walked, so nothing is dropped silently.
func anthropicNestedBlocks(v any, base string, depth int) []core.ContentBlock {
	// Levels walked, counting from the first nested array. Beyond this the
	// content is described rather than projected.
	const maxNestedDepth = 4
	arr, ok := v.([]any)
	if !ok || depth > maxNestedDepth {
		// Never serialise the subtree: a document block's payload is base64,
		// and marshalling it here would put megabytes of it into a text
		// block that feeds compliance scanning — the exact defect this whole
		// change removes, re-created behind a depth gate. Describe the shape
		// instead, so nothing is silently dropped and no payload leaks.
		return []core.ContentBlock{{Type: core.ContentText, Text: anthropicShapeSummary(v, depth)}}
	}
	out := make([]core.ContentBlock, 0, len(arr))
	for i, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, anthropicPartAtDepth(m, locator.JoinPath(base, i), depth+1)...)
	}
	return out
}

// anthropicShapeSummary names what a value contained without reproducing
// any of it. Used where content is too deeply nested to project: the reader
// learns that something was there and what kind, and no payload moves.
func anthropicShapeSummary(v any, depth int) string {
	switch t := v.(type) {
	case []any:
		kinds := map[string]int{}
		for _, it := range t {
			if m, ok := it.(map[string]any); ok {
				k, _ := m["type"].(string)
				if k == "" {
					k = "untyped"
				}
				kinds[k]++
			} else {
				kinds["non-object"]++
			}
		}
		names := make([]string, 0, len(kinds))
		for k, n := range kinds {
			names = append(names, fmt.Sprintf("%s x%d", k, n))
		}
		sort.Strings(names)
		return payloadSafeText(
			fmt.Sprintf("[nested content beyond depth %d not projected: ", depth),
			strings.Join(names, ", ")+"]")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return payloadSafeText("[nested content not projected: object with keys ", strings.Join(keys, ", ")+"]")
	case nil:
		return "[nested content absent]"
	default:
		return fmt.Sprintf("[nested content not projected: %T]", v)
	}
}

// anthropicSourceMedia reads the `source` envelope shared by image and
// document parts. locator is the gjson path of this part; the data field
// hangs one level below it.
//
// The three byte-bearing source variants map onto three custody states:
// base64 bytes are captured, a url is external, a file_id is provider-held.
// The two text-bearing document variants never reach here — the caller
// renders those as text so they stay scannable.
//
// modality, when non-empty, overrides mime-derived inference. It is set for
// image parts, whose url and file-id variants document no media_type: with
// nothing to infer from, an image would otherwise be classified as a file,
// which renders the wrong card and drops the vision requirement that
// capability routing keys off.
func anthropicSourceMedia(part map[string]any, path, modality string) *core.MediaRef {
	src, _ := part["source"].(map[string]any)
	if src == nil {
		m := modality
		if m == "" {
			m = core.ModalityFile
		}
		return &core.MediaRef{Modality: m, Source: core.MediaAbsent}
	}
	mime, _ := src["media_type"].(string)
	switch st, _ := src["type"].(string); st {
	case "base64":
		data, _ := src["data"].(string)
		ref := capturedMedia(mime, data, locator.JSON(locator.JoinSuffix(path, "source.data")))
		if modality != "" {
			// The block type is the authority on modality: an `image` block
			// is an image whatever its media_type claims. Deferring to the
			// mime here let an odd declared type drop the vision
			// requirement that capability routing keys off.
			ref.Modality = modality
		}
		return ref
	case "url":
		u, _ := src["url"].(string)
		return externalMedia(mime, u, modality)
	case "file":
		id, _ := src["file_id"].(string)
		return providerRefMedia(mime, id, modality)
	default:
		m := modality
		if m == "" {
			m = modalityFromMime(mime)
		}
		return &core.MediaRef{Modality: m, Mime: mime, Source: core.MediaAbsent}
	}
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
	// OutputTokensDetails carries the wire's own thinking-token count; when
	// present it beats any character-length estimate.
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (n *AnthropicMessagesNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp anthropicResponse
	if err := decodeLenient(raw, &resp); err != nil {
		return zeroAnthropic(meta), fmt.Errorf("anthropic-messages: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "anthropic-messages",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		FinishReason:     resp.StopReason,
	}
	blocks := anthropicDecodeContent(resp.Content, "content")
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
// Anthropic counts thinking tokens inside output_tokens; responses that carry
// usage.output_tokens_details.thinking_tokens report the exact split and that
// value is used as-is. Responses without it fall back to a heuristic:
// reasoningChars × 2/7 (chars/3.5, the estimator's default Anthropic tokenizer) —
// approximate (±15%) but a non-zero signal beats misclassifying every Claude row
// as "no reasoning". Neither affects the billed total — output_tokens already
// includes the thinking tokens.
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
	if raw.OutputTokensDetails.ThinkingTokens > 0 {
		v := raw.OutputTokensDetails.ThinkingTokens
		u.ReasoningTokens = &v
	} else if reasoningChars > 0 {
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
	// The wire's own thinking split rides output_tokens_details on the same
	// events; the non-streaming decode already prefers it, and a stream that
	// dropped it reported reasoning turns as unreasoned.
	if det, ok := raw["output_tokens_details"].(map[string]any); ok {
		if i := intFromAny(det["thinking_tokens"]); i > 0 {
			v := i
			prev.ReasoningTokens = &v
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
