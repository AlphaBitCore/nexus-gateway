// Package extract provides reusable pattern-extraction utilities for the
// normalize package's Tier-2 fallback. It implements:
//
//   - SSE frame walker (WalkSSE) — shared with future refactors of the
//     openai_chat / anthropic_messages / gemini_generate stream paths.
//   - JSON-patch op accumulator — handles ChatGPT-flavoured `add` /
//     `append` / `replace` / `remove` / `patch` ops over an in-memory
//     document tree. Re-usable by any per-host adapter that ships a
//     similar delta stream (not just chatgpt-web).
//   - Multi-spec chat-shape probe — recognises the consumer-web
//     shapes (ChatGPT-web, claude.ai) plus the flat-prompt legacy
//     completions shape by byte-level locator + role-path +
//     content-path inspection. Standard-API wires are decoded by the
//     Tier-1 codecs, never patterned here. Returns a Confidence score
//     the Coordinator uses to decide whether the probe's result is
//     the final audit answer or Tier-3 verbatim should win.
//
// The package is intentionally narrow: it does NOT know about
// providers.Format, HookConfig, IamPolicy, or the audit envelope. It
// consumes raw bytes + a hint and produces a normalize.NormalizedPayload.
// This keeps it import-cycle-safe (extract → normalize, never the
// reverse).
package extract

import "github.com/goccy/go-json"

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
	ToolsPath       string
	SignatureFields []string
}

// ChatResponseSpec mirrors ChatSpec for response bodies. The probe
// folds the body as a JSON-Patch SSE stream (JSONPatchAccumulator) —
// the only response framing Tier 2 recognises — and applies the
// spec's paths on the assembled accumulator state.
//
//   - AssistantTextPath — gjson path on the assembled state pointing
//     at the assistant's final text content. For ChatGPT-web it's
//     "message.content.parts.0".
//   - SignatureFields — keys probed against each RAW frame's data
//     JSON; at least one hit is required before the spec may claim
//     the stream (identification), independent of the patch-coverage
//     confidence.
//   - ModelFramePaths — gjson paths probed against each RAW frame's
//     data JSON for the model identifier (consumer-web streams carry
//     it as frame metadata, e.g. chatgpt-web's `model_slug`, not as a
//     top-level document field). The first hit wins and populates
//     ChatDetection.Model → NormalizedPayload.Model. Best-effort and
//     confidence-neutral, like AssistantTextPath.
type ChatResponseSpec struct {
	ID                string
	AssistantTextPath string
	SignatureFields   []string
	ModelFramePaths   []string
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
	ToolsRaw      json.RawMessage
	// MessageRoles + MessageContents are the role-aware structured form,
	// preserved so the normalizer wrapper can fill NormalizedPayload.Messages
	// without re-parsing.
	MessageRoles    []string
	MessageContents []string
}
