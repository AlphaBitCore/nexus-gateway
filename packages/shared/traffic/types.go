// Package traffic provides the adapter interface, domain/path matching,
// and content extraction framework for the traffic interception pipeline
// shared by Transparent Proxy and Desktop Agent.
package traffic

import "errors"

// FilterResult is the outcome of the domain+path filter stage.
type FilterResult int

const (
	// Process means the request enters the hook pipeline.
	Process FilterResult = iota
	// Passthrough means lightweight audit only (no hook pipeline).
	Passthrough
	// Block means the request is blocked outright.
	Block
)

// String returns the human-readable name.
func (f FilterResult) String() string {
	switch f {
	case Process:
		return "PROCESS"
	case Passthrough:
		return "PASSTHROUGH"
	case Block:
		return "BLOCK"
	default:
		return "UNKNOWN"
	}
}

// NormalizedContent is the provider-agnostic content representation
// produced by an Adapter's Extract* methods. Hooks consume this.
type NormalizedContent struct {
	// Segments are the extracted text segments (e.g. message contents).
	// Order is positionally aligned with the schema slots that
	// RewriteRequestBody / RewriteResponseBody walk back over, so
	// hook-modified Segments can be written in place.
	Segments []string
	// ReasoningSegments are advisory text segments produced by reasoning
	// or extended-thinking outputs (Anthropic thinking_delta + thinking
	// content blocks, OpenAI / DeepSeek delta.reasoning_content). Kept
	// separate from Segments because:
	//   - Reasoning text is not part of the assistant's user-visible
	//     content, so audit transcripts and UI rendering should treat it
	//     differently;
	//   - There is no stable rewrite slot for streaming reasoning
	//     deltas, so the Rewrite path intentionally ignores this list;
	//   - Compliance hooks that want to scan reasoning for PII can opt
	//     in by reading both Segments and ReasoningSegments.
	ReasoningSegments []string
	// ToolCallSegments are serialized JSON fragments — one per tool /
	// function invocation emitted by the model. Each entry carries the
	// adapter's raw `tool_call` (or `function_call` / `tool_use`) object
	// as JSON so downstream hooks can:
	//   - Detect MCP-formatted tool requests carried inside provider
	//     responses (compliance scanners walk the JSON for known MCP
	//     tool name prefixes);
	//   - Inspect tool arguments for PII / secrets / policy violations;
	//   - Drive cost / audit accounting separately from text completions.
	// Kept separate from Segments because tool_call content is not text
	// the user sees and has no stable rewrite slot — Rewrite walks
	// Segments only and intentionally ignores this list. Adapters that
	// do not parse tool calls leave this nil; consumers must treat nil
	// and empty as identical.
	ToolCallSegments []string
	// Extra holds **unrecognised top-level fields** — anything in the
	// provider body that the adapter did not consume into Segments,
	// ReasoningSegments, ToolCallSegments, or Metadata. Each entry is
	// the raw JSON value (as a string) keyed by the original field
	// name. This is the safety net against silent data loss when a
	// provider ships a new spec field (citations, grounding metadata,
	// reasoning summary, web_search_options, audio output, …) before
	// the adapter is updated to recognise it.
	//
	// Adapters that recognise every key on the request leave this nil.
	// Adapters that miss keys MUST drop the unrecognised raw JSON here
	// so compliance hooks doing defence-in-depth see new fields the
	// next time the spec evolves. Consumers must treat nil and empty
	// as identical.
	Extra map[string]string
	// Metadata holds adapter-specific key-value pairs (e.g. model name, token count hint).
	Metadata map[string]string
	// Partial is true when extraction succeeded but some content was unreadable.
	Partial bool
}

// Sentinel errors returned by Adapter.Extract* methods.
var (
	// ErrUnknownSchema means the body structure is unrecognised — top-level
	// required fields are missing. The caller should apply the domain's
	// unmatched_action policy.
	ErrUnknownSchema = errors.New("traffic: unknown schema")

	// ErrMalformed means the body is not valid JSON or is otherwise corrupt.
	// Default handling: reject (treat as hostile).
	ErrMalformed = errors.New("traffic: malformed body")

	// ErrPartial means extraction partially succeeded. The returned
	// NormalizedContent.Partial is true and downstream hooks should account
	// for missing data.
	ErrPartial = errors.New("traffic: partial extraction")

	// ErrRewriteUnsupported means the adapter cannot reconstruct the
	// provider wire format from NormalizedContent. Callers that receive
	// this sentinel should forward the original body unchanged and emit
	// a warn-level log instead of failing the request.
	ErrRewriteUnsupported = errors.New("traffic: adapter does not support body rewrite")
)
