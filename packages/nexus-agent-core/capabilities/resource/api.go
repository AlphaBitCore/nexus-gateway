package resource

import (
	"fmt"
	"sort"
)

// Public catalog accessors for the TUI's /resource cascade. The embedded OpenAPI
// catalog lives in this package (it backs the agent's resource_* tools); the
// cascade is a deterministic, local, zero-LLM front-end over the SAME catalog +
// admin API, reaching every kind and every operation — at any nesting depth —
// through one operation-driven resolver instead of per-kind UI code.

// KindInfo is one kind plus a small summary for the picker. Verbs are the canonical
// CRUD/action verbs (a quick hint that may be empty for a reports/config kind);
// OpCount is the total operations the kind exposes, so a non-CRUD kind never renders
// blank.
type KindInfo struct {
	Kind    string   `json:"kind"`
	Verbs   []string `json:"verbs"`
	OpCount int      `json:"opCount"`
}

// OperationInfo is one operation exposed to the cascade: enough to render it in a
// menu, drill into it (binding the next path param), and resolve it to an admin
// call. It is the TUI mirror of the internal Operation.
type OperationInfo struct {
	Kind        string   `json:"kind"`
	OperationID string   `json:"operationId"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Label       string   `json:"label"`
	Verb        string   `json:"verb,omitempty"` // canonical verb (list/get/create/update/delete/action:x) or ""
	Params      []string `json:"params,omitempty"`
	Mutating    bool     `json:"mutating"`
}

func toOperationInfo(op Operation) OperationInfo {
	return OperationInfo{
		Kind:        op.Kind,
		OperationID: op.OperationID,
		Method:      op.Method,
		Path:        op.Path,
		Label:       op.Label(),
		Verb:        op.CanonicalVerb(),
		Params:      op.Params,
		Mutating:    op.Mutating(),
	}
}

// Kinds returns the catalog kinds (sorted by name) for the cascade picker.
func Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(resCatalog.Kinds))
	for _, k := range resCatalog.Kinds {
		out = append(out, KindInfo{Kind: k.Kind, Verbs: k.canonicalVerbs(), OpCount: len(k.Operations)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Operations returns every operation a kind exposes, in catalog order, so the
// cascade can present and drill the full set (not just canonical CRUD).
func Operations(kind string) []OperationInfo {
	rk, ok := resIdx[kind]
	if !ok {
		return nil
	}
	ops := rk.operations()
	out := make([]OperationInfo, 0, len(ops))
	for _, op := range ops {
		out = append(out, toOperationInfo(op))
	}
	return out
}

// ResolveOperation resolves a (kind, operationId, path params) tuple to the HTTP
// method + concrete path for a direct admin call, plus whether it mutates (so the
// cascade gates it behind confirm). params fills every {placeholder} by name; a
// missing param is an error (no half-substituted path ever reaches the server).
func ResolveOperation(kind, operationID string, params map[string]string) (method, path string, mutating bool, err error) {
	op, ok := FindOp(kind, operationID)
	if !ok {
		return "", "", false, errUnknownOperation{kind: kind, op: operationID}
	}
	path, err = SubstituteParams(op.Path, params)
	if err != nil {
		return "", "", op.Mutating(), err
	}
	return op.Method, path, op.Mutating(), nil
}

// SearchInfos ranks operations across all kinds against a free-text query
// (code-level match), for a global operation search in the cascade.
func SearchInfos(query string, limit int) []OperationInfo {
	cands := Search(query, limit)
	out := make([]OperationInfo, 0, len(cands))
	for _, op := range cands {
		out = append(out, toOperationInfo(op))
	}
	return out
}

// FieldInfo is one input field of an operation (a query/path parameter or a body
// field), with enough metadata for the cascade to render an input control: an enum
// becomes a choice picker, a free field a text input.
type FieldInfo struct {
	Name     string   `json:"name"`
	Type     string   `json:"type,omitempty"`
	Required bool     `json:"required"`
	Enum     []string `json:"enum,omitempty"`
	In       string   `json:"in,omitempty"` // "path" / "query" for params; "" for body fields
}

// OperationSchema is the input surface of one operation: its path/query parameters
// and request-body fields, distilled from the embedded OpenAPI spec. The cascade
// uses it to build filter forms (query params) and write forms (body fields).
type OperationSchema struct {
	OperationID string      `json:"operationId"`
	Method      string      `json:"method"`
	Path        string      `json:"path"`
	Params      []FieldInfo `json:"params,omitempty"`
	Body        []FieldInfo `json:"body,omitempty"`
}

// DescribeOperation returns the input schema (params + body fields) for one
// operation, distilled from the kind's embedded OpenAPI spec. ok is false for an
// unknown kind/operation or an unreadable spec (an empty schema is still safe to
// render — the cascade falls back to a raw body editor).
func DescribeOperation(kind, operationID string) (OperationSchema, bool) {
	rk, found := resIdx[kind]
	if !found {
		return OperationSchema{}, false
	}
	raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + rk.File)
	if err != nil {
		return OperationSchema{}, false
	}
	d, err := distillKind(rk, raw)
	if err != nil {
		return OperationSchema{}, false
	}
	for _, op := range d.Operations {
		if op.OperationID != operationID {
			continue
		}
		s := OperationSchema{OperationID: op.OperationID, Method: op.Method, Path: op.Path}
		for _, p := range op.Params {
			s.Params = append(s.Params, FieldInfo{
				Name: p.Name, Type: p.Type, Required: p.Required, Enum: enumStrings(p.Enum), In: p.In,
			})
		}
		for _, f := range op.Body {
			s.Body = append(s.Body, FieldInfo{
				Name: f.Name, Type: f.Type, Required: f.Required, Enum: enumStrings(f.Enum),
			})
		}
		return s, true
	}
	return OperationSchema{}, false
}

// enumStrings renders an OpenAPI enum ([]any) as display strings for a choice
// picker. Non-string members are formatted with %v (numbers/bools are valid enum
// values in OpenAPI 3.1).
func enumStrings(vals []any) []string {
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if s, ok := v.(string); ok {
			out = append(out, s)
		} else {
			out = append(out, fmt.Sprintf("%v", v))
		}
	}
	return out
}

// errUnknownOperation is returned by ResolveOperation for a (kind, operationId)
// that is not in the catalog.
type errUnknownOperation struct{ kind, op string }

func (e errUnknownOperation) Error() string {
	return "no operation " + e.op + " on kind " + e.kind
}
