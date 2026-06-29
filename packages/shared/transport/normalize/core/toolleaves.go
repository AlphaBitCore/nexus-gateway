package core

import (
	"sort"
	"strings"

	"github.com/goccy/go-json"
)

// toolUseAddrMarker identifies a TransformSpan that targets a tool-call
// argument leaf (address shape messages.<i>.content.<j>.toolUse.input.<n>).
const toolUseAddrMarker = ".toolUse.input."

// ToolCallArgsFromPayload masks p with spans and returns, in document order
// (the order the OpenAI codec emitted ContentToolUse blocks from wire
// tool_calls[]), one entry per ContentToolUse block so a wire rewriter can zip
// ToolCallArgs[i] onto the i-th tool call.
//
// It returns nil when no span addresses a tool-call leaf, so a response with no
// tool-argument redaction incurs zero wire churn. When at least one tool leaf
// IS masked, the returned slice carries:
//   - for a block a masking span actually targeted: the re-marshaled masked
//     Input JSON (R4: the whole map is re-marshaled, never sjson-edited inside
//     the arguments string);
//   - for every untouched SIBLING block: the EMPTY STRING — a sentinel telling
//     the rewriter to leave that call's wire `arguments` byte-for-byte intact.
//     This avoids clobbering an innocent sibling: re-marshaling an unmasked
//     map would reorder keys / lose float precision, and a nil-Input call would
//     be rewritten to "{}" (data loss). Only the calls that genuinely changed
//     are rewritten.
//
// nil is also returned when every tool span was skipped (e.g. a stale ordinal
// fail-safe-skipped in ApplySpans), so a slice of all-empty sentinels never
// reaches the wire.
func ToolCallArgsFromPayload(p NormalizedPayload, spans []TransformSpan) []string {
	// Collect the set of content blocks (message index, content index) that a
	// tool-leaf span actually addresses, so only those blocks are rewritten.
	targeted := map[[2]int]bool{}
	for i := range spans {
		if mi, ci, ok := toolUseBlockOf(spans[i].ContentAddress); ok {
			targeted[[2]int{mi, ci}] = true
		}
	}
	if len(targeted) == 0 {
		return nil
	}
	masked, _ := ApplySpans(p, spans)
	var out []string
	anyMasked := false
	for mi := range masked.Messages {
		for ci := range masked.Messages[mi].Content {
			b := masked.Messages[mi].Content[ci]
			if b.Type != ContentToolUse {
				continue
			}
			// Untouched sibling: leave its wire arguments byte-for-byte intact.
			if !targeted[[2]int{mi, ci}] {
				out = append(out, "")
				continue
			}
			input := map[string]any{}
			if b.ToolUse != nil && b.ToolUse.Input != nil {
				input = b.ToolUse.Input
			}
			raw, err := json.Marshal(input)
			if err != nil {
				// Fail safe: leave the wire arguments untouched rather than
				// corrupt them with a partial / invalid re-marshal.
				out = append(out, "")
				continue
			}
			out = append(out, string(raw))
			anyMasked = true
		}
	}
	if !anyMasked {
		return nil
	}
	return out
}

// toolUseBlockOf parses a tool-use leaf ContentAddress
// (messages.<i>.content.<j>.toolUse.input.<n>) and returns the (message index,
// content index) of the block it targets. ok is false for any address that is
// not a tool-use leaf address.
func toolUseBlockOf(addr string) (mi, ci int, ok bool) {
	if !strings.Contains(addr, toolUseAddrMarker) {
		return 0, 0, false
	}
	parts := strings.Split(addr, ".")
	// messages.<i>.content.<j>.toolUse.input.<n>
	if len(parts) != 7 || parts[0] != "messages" || parts[2] != "content" ||
		parts[4] != "toolUse" || parts[5] != "input" {
		return 0, 0, false
	}
	i, err := parseInt(parts[1])
	if err != nil {
		return 0, 0, false
	}
	j, err := parseInt(parts[3])
	if err != nil {
		return 0, 0, false
	}
	return i, j, true
}

// ToolLeaf is one string leaf discovered inside a ToolUse.Input tree by
// ToolUseStringLeaves. Ordinal is the leaf's position in the deterministic
// depth-first walk (0-based); Value is the string content at that leaf.
//
// Ordinal is the stable address used across the three tool-use sinks
// (detection projection, redaction addressing, storage apply) so they all
// agree on which leaf a span targets regardless of Go's randomized map
// iteration — every sink re-derives the same ordering from this one walk.
type ToolLeaf struct {
	Ordinal int
	Value   string
}

// ToolUseStringLeaves returns the string-valued leaves of a ToolUse.Input
// tree in a deterministic depth-first order. The walk is the single source
// of truth shared by projection (detection), addressedSegments (redaction
// addressing), and resolveTextRef (storage write-back); because all three
// derive leaf order from this one function, the ordinal addressing is
// skew-proof by construction (R1).
//
// Ordering rules:
//   - object: keys sorted with sort.Strings, visited in that order;
//   - array: elements visited by ascending index;
//   - leaf: only string values are emitted; numbers, bools, and null are
//     skipped (they cannot carry text PII a regex hook would mask, and a
//     non-string leaf has no byte range to redact).
//
// Ordinals are assigned sequentially across the whole tree in walk order,
// counting STRING leaves only — a number/bool/null does NOT consume an
// ordinal, so the ordinal space matches exactly what projection emits.
func ToolUseStringLeaves(input map[string]any) []ToolLeaf {
	if len(input) == 0 {
		return nil
	}
	var out []ToolLeaf
	walkToolValue(input, &out)
	return out
}

// walkToolValue recurses through v in deterministic order, appending one
// ToolLeaf per string leaf. The ordinal is the current length of *out, so
// leaves are numbered 0,1,2,… in visit order.
func walkToolValue(v any, out *[]ToolLeaf) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkToolValue(t[k], out)
		}
	case []any:
		for i := range t {
			walkToolValue(t[i], out)
		}
	case string:
		*out = append(*out, ToolLeaf{Ordinal: len(*out), Value: t})
	}
}

// toolLeafLen returns the byte length of the ordinal-th string leaf in
// input, read-only. The bool is false when the ordinal is out of range.
func toolLeafLen(input map[string]any, ordinal int) (int, bool) {
	leaves := ToolUseStringLeaves(input)
	if ordinal < 0 || ordinal >= len(leaves) {
		return 0, false
	}
	return len(leaves[ordinal].Value), true
}

// toolLeafRef locates the ordinal-th string leaf inside input — using the
// exact same deterministic walk as ToolUseStringLeaves — and returns a
// *string view of a boxed copy plus registers a deferred write-back into
// the live nested map/slice. The bool is false when the ordinal is out of
// range, so a stale or malformed address fail-safe-skips (the caller
// reports it in `skipped`) rather than panicking.
//
// The returned pointer addresses a local cell, mirroring mapEntryRef: the
// caller in applyToAddress reads it once and writes it once, then
// writeCtx.flush commits the final value into the tree via the closure
// captured against the parent container (a map[string]any keyed by string,
// or an []any indexed by int — both reference types, so the write lands in
// the same tree resolveTextRef walked). The closure is recorded on the
// per-ApplySpans-call writeCtx, never a package global, so concurrent
// ApplySpans calls cannot drop each other's tool-leaf write-backs.
func toolLeafRef(input map[string]any, ordinal int, wc *writeCtx) (*string, bool) {
	if input == nil || ordinal < 0 {
		return nil, false
	}
	idx := 0
	var found bool
	var ref *string
	var visit func(set func(string), v any)
	visit = func(set func(string), v any) {
		if found {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {

				visit(func(nv string) { t[k] = nv }, t[k])
				if found {
					return
				}
			}
		case []any:
			for i := range t {

				visit(func(nv string) { t[i] = nv }, t[i])
				if found {
					return
				}
			}
		case string:
			if idx == ordinal {
				cell := t
				ref = &cell
				wc.treeWrites = append(wc.treeWrites, func() { set(*ref) })
				found = true
				return
			}
			idx++
		}
	}
	// Top container is always a map; its own setter is never invoked
	// (a map is not itself a string leaf).
	visit(func(string) {}, input)
	if !found {
		return nil, false
	}
	return ref, true
}

// deepCopyJSONMap deep-copies a decoded-JSON object tree (map[string]any
// with nested map[string]any / []any / scalars), so a clone can be mutated
// without aliasing the source. Returns nil for a nil/empty input.
func deepCopyJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyJSONValue(v)
	}
	return out
}

func deepCopyJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyJSONMap(t)
	case []any:
		cp := make([]any, len(t))
		for i := range t {
			cp[i] = deepCopyJSONValue(t[i])
		}
		return cp
	default:
		// Scalars (string, float64, bool, nil) are immutable values — copy
		// by assignment.
		return t
	}
}
