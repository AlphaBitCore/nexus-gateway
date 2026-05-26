// Package extract provides reusable pattern-extraction utilities for the
// normalize package's Tier-2 fallback. It implements:
//
//   - SSE frame walker (WalkSSE) — shared with future refactors of the
//     openai_chat / anthropic_messages / gemini_generate stream paths.
//   - JSON-patch op accumulator — handles ChatGPT-flavoured `add` /
//     `append` / `replace` / `remove` / `patch` ops over an in-memory
//     document tree. Re-usable by any per-host adapter that ships a
//     similar delta stream (not just chatgpt-web).
//   - Multi-spec chat-shape probe — recognises OpenAI / Anthropic /
//     Gemini / ChatGPT-web / Anthropic-web / completions-legacy
//     request and response shapes by byte-level locator + role-path +
//     content-path inspection. Returns a Confidence score the
//     Coordinator uses to decide whether the probe's result is the
//     final audit answer or Tier-3 verbatim should win.
//
// The package is intentionally narrow: it does NOT know about
// providers.Format, HookConfig, IamPolicy, or the audit envelope. It
// consumes raw bytes + a hint and produces a normalize.NormalizedPayload.
// This keeps it import-cycle-safe (extract → normalize, never the
// reverse).
package extract

import "encoding/json"

// JSONPatchOp is a single operation in a ChatGPT-flavoured JSON Patch
// stream. The wire shape uses single-letter fields for terseness:
//
//	{ "p": "/message/content/parts/0", "o": "append", "v": "..." }
//
// `o` (Op) values:
//
//   - "add" — set value at path, creating intermediate objects/arrays.
//     If path is "" or missing, replaces the whole document root.
//   - "append" — append string value to existing string at path. Path
//     MUST already point at a string (or be implicit via shorthand —
//     see below). Future appends without `p` re-use the most-recent
//     append path.
//   - "replace" — overwrite value at path (string, number, object, etc.).
//   - "remove" — delete the key/index at path.
//   - "patch" — `v` is a JSON array of nested JSONPatchOp objects,
//     applied in order. Used by ChatGPT to bundle terminal appends
//     into one frame at end-of-stream.
//
// A frame that arrives with no `p` AND no `o` is treated as a shorthand
// continuation of the previous `append` at the previous path — common
// in ChatGPT SSE where successive deltas to the same parts[0] string
// omit redundant routing fields.
type JSONPatchOp struct {
	Path string          `json:"p,omitempty"`
	Op   string          `json:"o,omitempty"`
	Val  json.RawMessage `json:"v"`
}

// HasShape reports whether a parsed JSON object looks like a
// JSONPatchOp (has at least `v`, and optionally `p` + `o`). Used by
// extractors to decide whether to feed a frame into an accumulator
// or treat it as a self-contained chat completion message.
func (op JSONPatchOp) HasShape() bool {
	return len(op.Val) > 0
}

// ContentShape describes how a spec's content-path returns text.
type ContentShape int

const (
	// ContentShapeString — content at the role-path is a plain string.
	// Example: OpenAI `{role, content: "hi"}`.
	ContentShapeString ContentShape = iota
	// ContentShapeBlockArray — content is an array of content blocks,
	// each with `type` + `text` (or other typed fields). Example:
	// Anthropic Messages `{role, content: [{type:"text", text:"hi"}]}`.
	ContentShapeBlockArray
	// ContentShapeStringArray — content is an array of strings (or a
	// JSON node whose direct array elements are strings). Example:
	// ChatGPT-web `{author:{role}, content:{parts:["hi"]}}`.
	ContentShapeStringArray
	// ContentShapeNestedTextArray — content is an array of objects each
	// carrying a `text` field. Example: Gemini `{role, parts:[{text:"hi"}]}`.
	ContentShapeNestedTextArray
)

// ChatSpec describes one known request-body chat shape. The probe in
// probe.go iterates KnownChatSpecs and scores each spec's match against
// the body bytes; the highest scorer above threshold wins.
//
// Fields:
//
//   - ID — stable identifier appended to NormalizedPayload.DetectedSpec
//     as `pattern:<id>` (e.g. "pattern:chatgpt-web"). Lets UI and
//     analytics name the recognised family.
//   - Locator — gjson path to the messages array (e.g. "messages" or
//     "contents"). When the locator hits AND the result is a non-empty
//     array, that's the strongest single signal (worth 0.4 confidence).
//   - RolePath — relative gjson path inside each message element pointing
//     at the role string. May be "role" (OpenAI / Anthropic) or
//     "author.role" (ChatGPT-web).
//   - ContentPath — relative gjson path inside each message element
//     pointing at the content. Shape governed by Shape field.
//   - Shape — how to interpret content at ContentPath.
//   - ModelPath — optional top-level gjson path to the model identifier
//     ("model" / "model_id" / "model_name"). When present, populates
//     NormalizedPayload.Model.
//   - SystemPath — optional top-level gjson path for separated system
//     prompt (some specs hoist it out of messages).
//   - ToolsPath — optional top-level path to the tools / functions
//     array, copied wholesale into NormalizedPayload.Tools.
//   - SignatureFields — additional optional probes (any field name
//     unique-ish to this spec) that bump confidence when present. E.g.
//     ChatGPT-web's distinctive `suggestion_type` / `chosen_suggestion`
//     fields; Anthropic's `anthropic_version`. Each hit adds 0.05.
type ChatSpec struct {
	ID              string
	Locator         string
	RolePath        string
	ContentPath     string
	Shape           ContentShape
	ModelPath       string
	SystemPath      string
	ToolsPath       string
	SignatureFields []string
}

// ChatResponseSpec mirrors ChatSpec for response bodies. Response specs
// can also carry an SSE flag — when true, the probe runs WalkSSE first
// and applies the spec's locator on the assembled accumulator state
// rather than the raw bytes.
//
//   - StreamFraming — "single-json" / "sse-event-data" / "ndjson". When
//     not single-json, the probe walks the stream into a state object
//     before applying ContentPath / RolePath.
//   - AccumulatorRule — "none" / "concat-text" / "json-patch". Names the
//     algorithm that turns a sequence of SSE data frames into a single
//     document tree. "concat-text" appends `delta.text` (Anthropic-
//     stream) / `delta.content` (OpenAI-stream) across frames.
//     "json-patch" routes each frame through JSONPatchAccumulator.
//   - AssistantTextPath — gjson path on the assembled state (or
//     non-stream body) pointing at the assistant's final text content.
//     For Anthropic this is "content.0.text"; for ChatGPT-web it's
//     "message.content.parts.0".
//   - UsagePath — optional gjson path to a Usage-shaped object (token
//     counts). Often absent on consumer surfaces; when present,
//     populates NormalizedPayload.Usage best-effort.
//   - FinishReasonPath — optional path to a finish/stop reason string.
//   - ModelPath — optional path to the model identifier reported in
//     the response (some specs echo it back in a header frame).
type ChatResponseSpec struct {
	ID                string
	StreamFraming     string
	AccumulatorRule   string
	AssistantTextPath string
	UsagePath         string
	FinishReasonPath  string
	ModelPath         string
	SignatureFields   []string
	// StreamDeltaPath, when set on a "concat-text" spec, is the
	// gjson path each SSE frame is checked against. Only frames where
	// this path resolves to a non-empty string count toward the
	// spec's frame counter (used in confidence scoring). Lets
	// per-format specs (OpenAI / Anthropic / Gemini) avoid
	// false-positive matches on each other's frame shapes.
	StreamDeltaPath string
}

// ChatDetection is the probe's result. Confidence in [0, 1] is what the
// Coordinator inspects.
type ChatDetection struct {
	Confidence    float64
	SpecID        string
	IsStream      bool
	IsResponse    bool // false=request, true=response
	UserPrompts   []string
	AssistantText string
	Model         string
	System        string
	ToolsRaw      json.RawMessage
	UsageRaw      json.RawMessage
	FinishReason  string
	// MessageRoles + MessageContents are the role-aware structured form,
	// preserved so the normalizer wrapper can fill NormalizedPayload.Messages
	// without re-parsing.
	MessageRoles    []string
	MessageContents []string
}
