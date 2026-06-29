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
	// ToolCallArgs carries the REWRITTEN (compliance-masked) `function.arguments`
	// JSON string for each function-type tool call, in the SAME order the
	// adapter's Extract* walk emits ContentToolUse blocks — i.e. the wire
	// `tool_calls[]` order. Unlike ToolCallSegments (a read-only audit
	// projection of the raw call), this slice is a REWRITE input:
	// RewriteRequestBody / RewriteResponseBody, when it is non-nil, zips
	// ToolCallArgs[k] onto the k-th tool call's `function.arguments`.
	//
	// nil means "no tool-argument redaction" — the rewriter leaves every tool
	// call untouched (zero churn on benign tool-calling traffic). When non-nil,
	// at least one call was masked; an entry is either a full JSON object string
	// produced by re-marshaling that call's masked Input map (never an sjson edit
	// inside the arguments string, so the result is always valid JSON), OR the
	// EMPTY STRING — a sentinel meaning "this sibling call had no masking span:
	// leave its wire arguments byte-for-byte untouched" (no re-marshal, so an
	// innocent call's float precision / key order / nil-arguments is preserved).
	// Order alignment with the wire is the caller's invariant, pinned by tests; a
	// short slice simply stops rewriting once exhausted.
	//
	// Only adapters that implement ToolArgMasker reconstruct this slice onto
	// their native wire; for any other adapter, a non-empty ToolCallArgs is an
	// undelivered mask, so callers MUST fail closed via GuardToolArgMasking
	// (legacy top-level chat `function_call.arguments` is NOT masked — the codec
	// drops that shape before canonicalization, so no ContentToolUse block, and
	// hence no ToolCallArgs entry, is ever produced for it).
	ToolCallArgs []string
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

// ToolArgMasker is the optional capability an Adapter implements when it can
// reconstruct compliance-masked NormalizedContent.ToolCallArgs onto its native
// wire inside RewriteRequestBody / RewriteResponseBody. Only adapters whose
// wire tool-call arguments map 1:1 onto the canonical re-marshaled Input
// (currently the OpenAI canonical adapter) implement it.
//
// An adapter that does NOT implement this interface walks only the text
// segments in its rewrite path and silently ignores ToolCallArgs — so if it is
// handed a non-empty ToolCallArgs (PII was masked in structured tool-call
// arguments) it would forward those arguments UNMASKED while the audit trail
// records them as redacted: a silent wire/audit divergence. Callers guard
// against that with GuardToolArgMasking, which fails closed before the
// divergence can occur.
type ToolArgMasker interface {
	// MasksToolCallArgs reports whether this adapter applies
	// NormalizedContent.ToolCallArgs onto its wire. Returns true for adapters
	// that mask tool-call arguments end-to-end.
	MasksToolCallArgs() bool
}

// ToolArgMaskingSupported reports whether a can deliver masked tool-call
// arguments onto its wire. An adapter that does not implement ToolArgMasker
// (or returns false) cannot, so callers must not trust a nil-error rewrite to
// have masked tool args.
func ToolArgMaskingSupported(a Adapter) bool {
	m, ok := a.(ToolArgMasker)
	return ok && m.MasksToolCallArgs()
}

// GuardToolArgMasking returns ErrRewriteUnsupported when content carries
// non-empty ToolCallArgs but adapter cannot mask them onto its native wire,
// so the caller fails closed (forwards the original body + stamps the disclosed
// degraded path) instead of forwarding tool-arg PII unmasked under a "fully
// redacted" audit record. Returns nil when there is nothing to guard (no
// tool-argument masking was requested) or the adapter supports masking.
//
// content.ToolCallArgs is non-nil only when at least one tool call was actually
// masked (ToolCallArgsFromPayload returns nil otherwise), so a non-empty slice
// always represents a real, undelivered redaction on a non-OpenAI wire.
func GuardToolArgMasking(adapter Adapter, content NormalizedContent) error {
	if len(content.ToolCallArgs) == 0 {
		return nil
	}
	if ToolArgMaskingSupported(adapter) {
		return nil
	}
	return ErrRewriteUnsupported
}
