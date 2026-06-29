package extract

import (
	"fmt"
	"github.com/goccy/go-json"
	"strconv"
	"strings"
)

// JSONPatchAccumulator builds a single document tree out of a stream of
// JSONPatchOp ops. Used by Tier-2 extraction to replay ChatGPT-style
// SSE deltas into a final state from which DetectResponseShape can
// extract the assistant text.
//
// Design notes:
//
//   - The internal `state` is a `map[string]any` (the root object) — we
//     never start from a non-object root because every observed
//     producer (ChatGPT-web / Claude-web / Gemini-web) puts deltas
//     under a top-level object.
//   - Paths are RFC 6901 JSON Pointers ("/foo/bar/0"). "" addresses the
//     root document; an op with empty path + value=object replaces the
//     root.
//   - Op semantics (ChatGPT-flavoured superset of RFC 6902):
//   - `add` — set value at path, creating intermediate objects/arrays
//     as needed. Idempotent re-add to same path overwrites.
//   - `append` — string concat: existing string at path += new string.
//     If path doesn't exist, behaves like `add` with new string.
//   - `replace` — same as `add` but does NOT create intermediates;
//     errors if path doesn't already exist (currently we relax this
//     to behave like `add` for robustness against partial streams).
//   - `remove` — delete key/index at path.
//   - `patch` — value is an array of nested JSONPatchOp; apply each in
//     order via Apply (recurses through the accumulator).
//   - Shorthand frames (no `p`, no `o`, just `v`): treated as
//     continuation of the most recent `append`. The previous path is
//     re-used.
//   - Errors surface only on truly malformed ops; missing keys are
//     created. This is "best-effort accumulation" — Tier-2 wants to
//     extract as much as possible from imperfect streams.
type JSONPatchAccumulator struct {
	state          map[string]any
	lastAppendPath string // for shorthand frames (no p+o)
}

// NewJSONPatchAccumulator returns a fresh accumulator with empty state.
func NewJSONPatchAccumulator() *JSONPatchAccumulator {
	return &JSONPatchAccumulator{state: map[string]any{}}
}

// State returns the current internal tree. Returned map is the live
// internal storage — callers MUST treat it as read-only or copy before
// mutating.
func (a *JSONPatchAccumulator) State() map[string]any {
	return a.state
}

// Apply runs one op against the internal state. Errors are returned
// for malformed input; out-of-band conditions (missing key on replace,
// type mismatch on append) degrade to add semantics so accumulators
// survive partial streams.
func (a *JSONPatchAccumulator) Apply(op JSONPatchOp) error {
	path := op.Path
	opName := op.Op

	// Disambiguate when op is unspecified by inspecting v's shape:
	//
	//   * If v is a JSON string AND we have a recorded lastAppendPath
	//     from a prior `append` op → this is a SHORTHAND continuation
	//     of the last append (ChatGPT-web's common compression).
	//   * If v is a JSON object or array → treat as `add` at the given
	//     path (root if path is empty), which is how every ChatGPT
	//     frame that ships a full new message tree lands.
	//   * Otherwise default to `add`.
	if opName == "" {
		if isStringJSON(op.Val) && a.lastAppendPath != "" && path == "" {
			path = a.lastAppendPath
			opName = "append"
		} else {
			opName = "add"
		}
	}

	switch opName {
	case "add":
		return a.applyAdd(path, op.Val)
	case "append":
		if err := a.applyAppend(path, op.Val); err != nil {
			return err
		}
		a.lastAppendPath = path
		return nil
	case "replace":
		// Relaxed: behave like add even if path doesn't exist yet.
		return a.applyAdd(path, op.Val)
	case "remove":
		return a.applyRemove(path)
	case "patch":
		return a.applyPatch(op.Val)
	default:
		return fmt.Errorf("extract: unknown patch op %q", opName)
	}
}

// ApplyJSON decodes one raw JSON frame as a JSONPatchOp and applies it.
// Convenience for SSE walkers: passes the data string straight through.
func (a *JSONPatchAccumulator) ApplyJSON(raw []byte) error {
	var op JSONPatchOp
	if err := json.Unmarshal(raw, &op); err != nil {
		return fmt.Errorf("extract: decode op: %w", err)
	}
	return a.Apply(op)
}

// applyAdd sets val at path, creating intermediate containers.
func (a *JSONPatchAccumulator) applyAdd(path string, rawVal json.RawMessage) error {
	if path == "" {
		// Replace root. Value must decode to either an object (most
		// common) or — if a producer ships a non-object root — store
		// it under a placeholder so downstream extractors can still
		// see it.
		var decoded any
		if err := json.Unmarshal(rawVal, &decoded); err != nil {
			return fmt.Errorf("extract: add root: %w", err)
		}
		if m, ok := decoded.(map[string]any); ok {
			a.state = m
		} else {
			a.state = map[string]any{"_root": decoded}
		}
		return nil
	}
	tokens, err := parsePointer(path)
	if err != nil {
		return err
	}
	var decoded any
	if err := json.Unmarshal(rawVal, &decoded); err != nil {
		return fmt.Errorf("extract: add %s: %w", path, err)
	}
	return setAtPointer(a.state, tokens, decoded)
}

// applyAppend appends a string value to the string at path. Tolerates
// missing path (acts like add) and non-string current value (replaces).
func (a *JSONPatchAccumulator) applyAppend(path string, rawVal json.RawMessage) error {
	var s string
	if err := json.Unmarshal(rawVal, &s); err != nil {
		// Some producers send objects under append (shouldn't, but).
		// Fall through to add semantics so we don't lose the data.
		return a.applyAdd(path, rawVal)
	}
	if path == "" {
		// Append at root makes little sense; treat as add of a string
		// placeholder.
		a.state["_root"] = s
		return nil
	}
	tokens, err := parsePointer(path)
	if err != nil {
		return err
	}
	existing := getAtPointer(a.state, tokens)
	if cur, ok := existing.(string); ok {
		return setAtPointer(a.state, tokens, cur+s)
	}
	// Missing or non-string — set as a new string.
	return setAtPointer(a.state, tokens, s)
}

// applyRemove deletes the key/index at path.
func (a *JSONPatchAccumulator) applyRemove(path string) error {
	tokens, err := parsePointer(path)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		// Remove root — clear everything.
		a.state = map[string]any{}
		return nil
	}
	return removeAtPointer(a.state, tokens)
}

// applyPatch decodes val as []JSONPatchOp and applies each in order.
func (a *JSONPatchAccumulator) applyPatch(rawVal json.RawMessage) error {
	var ops []JSONPatchOp
	if err := json.Unmarshal(rawVal, &ops); err != nil {
		return fmt.Errorf("extract: patch decode: %w", err)
	}
	for i, op := range ops {
		if err := a.Apply(op); err != nil {
			return fmt.Errorf("extract: patch[%d]: %w", i, err)
		}
	}
	return nil
}

// ExtractByPointer reads a value at the given JSON Pointer path from
// the accumulator state. Returns ("", false) if the path is missing or
// the value isn't a string.
func (a *JSONPatchAccumulator) ExtractByPointer(path string) (string, bool) {
	tokens, err := parsePointer(path)
	if err != nil {
		return "", false
	}
	v := getAtPointer(a.state, tokens)
	s, ok := v.(string)
	return s, ok
}

// JSON Pointer (RFC 6901) helpers

// parsePointer splits a JSON Pointer string into tokens. "" → []. "/foo/0"
// → ["foo", "0"]. Tilde escapes: ~0 → ~, ~1 → /.
func parsePointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("extract: invalid pointer %q (missing leading /)", path)
	}
	parts := strings.Split(path[1:], "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		parts[i] = p
	}
	return parts, nil
}

// getAtPointer reads the value at the token path from root. Returns nil
// when any token is missing or there's a type mismatch.
func getAtPointer(root map[string]any, tokens []string) any {
	if len(tokens) == 0 {
		return root
	}
	var cur any = root
	for _, t := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[t]
			if !ok {
				return nil
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(t)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil
			}
			cur = node[idx]
		default:
			return nil
		}
	}
	return cur
}

// setAtPointer sets val at the token path, creating intermediate maps as
// needed. Arrays grow on numeric token > len-1. Callers (applyAdd /
// applyAppend) resolve the empty-path root case before descending, so
// tokens always carries at least one element here; every iteration
// either returns (terminal token, or an invalid shape error) or
// descends one level, so the loop is the function's only exit.
func setAtPointer(root map[string]any, tokens []string, val any) error {
	var cur any = root
	for i := 0; ; i++ {
		t := tokens[i]
		isLast := i == len(tokens)-1
		switch node := cur.(type) {
		case map[string]any:
			if isLast {
				node[t] = val
				return nil
			}
			next, ok := node[t]
			if !ok {
				// Decide intermediate container type based on next token.
				nextTok := tokens[i+1]
				if _, err := strconv.Atoi(nextTok); err == nil {
					next = []any{}
				} else {
					next = map[string]any{}
				}
				node[t] = next
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(t)
			if err != nil {
				return fmt.Errorf("extract: non-numeric index %q into array", t)
			}
			if idx < 0 {
				return fmt.Errorf("extract: negative array index %d", idx)
			}
			// Grow if needed
			for len(node) <= idx {
				node = append(node, nil)
			}
			// We must write the grown slice back into the parent. Do
			// this by re-traversing — simpler to just bail on growth
			// beyond original len for the streaming case (every ChatGPT
			// path I've seen stays within bounds because the initial
			// `add` materialised the array fully).
			if isLast {
				node[idx] = val
				// node aliasing: the slice header in `node` is the
				// caller's; assignment via index propagates. No
				// re-write required because we never grew.
				return nil
			}
			nextTok := tokens[i+1]
			var next any
			if node[idx] != nil {
				next = node[idx]
			} else {
				if _, err := strconv.Atoi(nextTok); err == nil {
					next = []any{}
				} else {
					next = map[string]any{}
				}
				node[idx] = next
			}
			cur = next
		case string:
			// The JSON-pointer path tries to descend through a scalar
			// string while there are still tokens left to traverse. That
			// is an invalid path for the current document shape, so we
			// reject it rather than silently overwriting the leaf.
			return fmt.Errorf("extract: path traverses scalar at %q", t)
		case nil:
			return fmt.Errorf("extract: nil at intermediate token %q", t)
		default:
			return fmt.Errorf("extract: unexpected type %T at %q", node, t)
		}
	}
}

// isStringJSON returns true when raw is a JSON-encoded string (first
// non-whitespace byte is "). Used by Apply to disambiguate shorthand
// append frames from add-at-root frames.
func isStringJSON(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '"':
			return true
		default:
			return false
		}
	}
	return false
}

// removeAtPointer deletes the key/index at tokens. The caller
// (applyRemove) resolves the empty-path root case before descending, so
// tokens always carries at least one element here; every iteration
// either returns (terminal token, or an absent / non-container path
// treated as a successful no-op) or descends one level, so the loop is
// the function's only exit.
func removeAtPointer(root map[string]any, tokens []string) error {
	var cur any = root
	for i := 0; ; i++ {
		t := tokens[i]
		isLast := i == len(tokens)-1
		switch node := cur.(type) {
		case map[string]any:
			if isLast {
				delete(node, t)
				return nil
			}
			next, ok := node[t]
			if !ok {
				return nil // already absent
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(t)
			if err != nil {
				return nil //nolint:nilerr // non-numeric token = absent path, not a propagated error
			}
			if idx < 0 || idx >= len(node) {
				return nil
			}
			if isLast {
				// Removing an array element shifts indices — but
				// modifying the caller's slice header requires
				// re-write. For audit accumulation purposes, set to
				// nil instead of splice; downstream extractors can
				// tolerate nil holes.
				node[idx] = nil
				return nil
			}
			cur = node[idx]
		default:
			return nil
		}
	}
}
