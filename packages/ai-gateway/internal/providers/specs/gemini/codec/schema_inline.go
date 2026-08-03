package codec

import (
	"errors"
	"fmt"
	"strings"
)

// Gemini's Schema proto has no reference mechanism, so `$ref` cannot survive to
// the wire. Dropping it — the sanitizer's only other option — is not neutral:
// a `$ref` under `properties` leaves the property as `{}` while `required` still
// names it, and a `$ref` at the root leaves the whole schema empty, at which
// point the caller's declared contract is gone but `responseMimeType: json`
// still goes out. The model then returns arbitrary JSON with HTTP 200 and the
// caller has no way to detect it.
//
// Nested models are the single most common structured-output shape in the Python
// ecosystem — Pydantic's model_json_schema() emits `$defs` + `$ref` for any
// BaseModel field — so this is the mainstream path, not an edge.
//
// inlineSchemaRefs resolves every local `$ref` against the root's `$defs` /
// `definitions` and returns a schema with no references left, which the
// sanitizer can then process without losing anything.
//
// What it will NOT do is guess a shape. A reference it cannot resolve, and a
// recursive model that has no finite expansion, fail the request so the caller
// sees a loud, diagnosable error instead of a silent wrong answer — with ONE
// carve-out the caller opts into per call site:
//
// The carve-out class: a pointer outside the schema-native dictionaries
// this file indexes — any local pointer whose first segment is not
// $defs/definitions (including self-pointers like #/properties/x, which
// are never indexed here), or a remote URL. Observed live as
// `#/components/schemas/handler_DryRunContext` (2026-07-17, prod
// traffic_event proxy_error 400): tool schemas extracted from an OpenAPI
// document keep OpenAPI-style `#/components/...` pointers whose targets
// were never shipped with the schema, and the same tool declaration is
// accepted verbatim by the OpenAI and Anthropic wires — so a hard 400
// here turned one un-shipped dictionary into a permanently failing
// provider for that caller. In lenient mode (tool parameters) such a
// reference degrades: the caller's own $ref siblings are kept and walked,
// `type` defaults to "object" only when the siblings did not state one,
// and the reference is REPORTED via droppedRefs so the coercion reaches
// x-nexus-coerced — visible, not silent. A dangling pointer into a
// dictionary the document DOES carry (`#/$defs/Missing`) stays a hard
// error in both modes: the author used the schema-native mechanism and
// the schema is broken, which degrading would only mask.
//
// Strict mode (responseSchema / structured output) never degrades: there the
// schema IS the caller's output contract, and a loosened property is exactly
// the silent-wrong-answer class this file exists to end.
func inlineSchemaRefs(root any, lenient bool) (any, []string, error) {
	defs := map[string]any{}
	if m, ok := root.(map[string]any); ok {
		// Draft 2020-12 names it $defs; draft-07 and many generators emit
		// `definitions`. Both are local dictionaries with identical semantics.
		for _, bucket := range []string{"$defs", "definitions"} {
			if d, ok := m[bucket].(map[string]any); ok {
				for name, schema := range d {
					defs[bucket+"/"+name] = schema
				}
			}
		}
	}
	w := &inlineWalk{defs: defs, lenient: lenient}
	out, err := w.node(root, nil)
	if err != nil {
		return nil, nil, err
	}
	// The dictionaries have been folded into their use sites; leaving them would
	// only give the sanitizer unknown keys to drop.
	if m, ok := out.(map[string]any); ok {
		delete(m, "$defs")
		delete(m, "definitions")
	}
	return out, w.dropped, nil
}

// inlineWalk carries the walk's fixed inputs and collects the references the
// lenient mode degraded, in encounter order, for the caller's coercion report.
type inlineWalk struct {
	defs    map[string]any
	lenient bool
	dropped []string
}

// node walks one node. `stack` carries the refs currently being expanded,
// which is what makes a cycle detectable: a model that contains itself would
// otherwise expand forever.
func (w *inlineWalk) node(v any, stack []string) (any, error) {
	switch node := v.(type) {
	case map[string]any:
		if raw, ok := node["$ref"].(string); ok {
			target, err := w.resolve(raw)
			if err != nil {
				if w.lenient && errors.Is(err, errRefOutsideDictionaries) {
					w.dropped = append(w.dropped, raw)
					return w.degradedRefNode(node, stack)
				}
				return nil, err
			}
			for _, seen := range stack {
				if seen == raw {
					return nil, fmt.Errorf(
						"gemini: schema reference %q is recursive; Gemini's Schema proto has no reference mechanism and a recursive model has no finite expansion",
						raw,
					)
				}
			}
			expanded, err := w.node(target, append(stack, raw))
			if err != nil {
				return nil, err
			}
			// `$ref` may carry siblings (draft 2020-12 allows it; generators use
			// it for `description`). The siblings are the caller's overrides, so
			// they win over the referenced definition.
			merged := map[string]any{}
			if em, ok := expanded.(map[string]any); ok {
				for k, val := range em {
					merged[k] = val
				}
			}
			for k, val := range node {
				if k == "$ref" {
					continue
				}
				sub, err := w.node(val, stack)
				if err != nil {
					return nil, err
				}
				merged[k] = sub
			}
			return merged, nil
		}
		out := make(map[string]any, len(node))
		for k, val := range node {
			// The dictionaries themselves are not schema content; they are
			// dropped at the root once every use site has been expanded.
			if k == "$defs" || k == "definitions" {
				continue
			}
			sub, err := w.node(val, stack)
			if err != nil {
				return nil, err
			}
			out[k] = sub
		}
		return out, nil
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			sub, err := w.node(item, stack)
			if err != nil {
				return nil, err
			}
			out[i] = sub
		}
		return out, nil
	default:
		return v, nil
	}
}

// degradedRefNode is the lenient replacement for a reference whose target
// was never shipped. The caller's own $ref siblings are the one part of
// the contract that DID ship, and draft 2020-12 makes them overrides — so
// they are kept (and walked, so a resolvable nested reference inside a
// sibling still inlines). Only the missing definition itself is replaced,
// by defaulting `type` to "object" when the siblings did not state one.
// Sibling errors propagate: a dangling `#/$defs/...` pointer INSIDE a
// kept sibling still fails the request — the shipped-dictionary classes
// stay fatal wherever they appear. Nothing else is guessed.
func (w *inlineWalk) degradedRefNode(node map[string]any, stack []string) (any, error) {
	out := map[string]any{}
	for k, val := range node {
		if k == "$ref" {
			continue
		}
		sub, err := w.node(val, stack)
		if err != nil {
			return nil, err
		}
		out[k] = sub
	}
	if _, has := out["type"]; !has {
		out["type"] = "object"
	}
	return out, nil
}

// errRefOutsideDictionaries classifies the reference the document could never
// have resolved: a local pointer whose first segment is not one of the
// schema-native dictionaries this file indexes ($defs / definitions) — the
// OpenAPI `#/components/...` class — or a non-local (remote) reference. It is
// the only class the lenient mode may degrade; a DANGLING pointer into a
// dictionary the document does carry is a broken schema and stays fatal.
var errRefOutsideDictionaries = errors.New("reference target was not shipped with the schema")

// resolve accepts only local pointers into the root's own dictionaries.
// Anything else — a remote URL, a pointer into a part of the document we do
// not index — cannot be fetched or resolved here, and guessing a SHAPE is what
// this whole file exists to avoid.
func (w *inlineWalk) resolve(ref string) (any, error) {
	key := strings.TrimPrefix(ref, "#/")
	if key == ref {
		return nil, fmt.Errorf("gemini: schema reference %q is not a local pointer; only #/$defs/... and #/definitions/... can be resolved: %w", ref, errRefOutsideDictionaries)
	}
	if target, ok := w.defs[key]; ok {
		return target, nil
	}
	if bucket, _, ok := strings.Cut(key, "/"); ok && (bucket == "$defs" || bucket == "definitions") {
		// The document carries (or should carry) this dictionary and the
		// pointer dangles: the schema itself is broken. Always fatal.
		return nil, fmt.Errorf("gemini: schema reference %q does not resolve against the schema's own $defs/definitions", ref)
	}
	return nil, fmt.Errorf("gemini: schema reference %q points outside the schema's own $defs/definitions and its target was not shipped with the schema: %w", ref, errRefOutsideDictionaries)
}
