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

// KindInfo is one kind plus a small summary for the picker. Capabilities is the
// kind's TOTAL capability profile (capability.go): deduplicated, full-CRUD
// collapsed to "crud", and never empty — a reports kind says `report`, a
// singleton-config kind says `config`, instead of the silent column the old
// canonical-verbs-only hint produced.
type KindInfo struct {
	Kind         string   `json:"kind"`
	Capabilities []string `json:"capabilities"`
	OpCount      int      `json:"opCount"`
}

// OperationInfo is one operation exposed to the cascade: enough to render it in a
// menu, drill into it (binding the next path param), and resolve it to an admin
// call. It is the TUI mirror of the internal Operation. Summary is the operation's
// OpenAPI summary from the init-time memo, so menus read "Set node config
// override" instead of a synthesized path-tail label.
type OperationInfo struct {
	Kind        string   `json:"kind"`
	OperationID string   `json:"operationId"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Label       string   `json:"label"`
	Summary     string   `json:"summary,omitempty"`
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
		Summary:     distilledIdx[[2]string{op.Kind, op.OperationID}].Summary,
		Verb:        op.CanonicalVerb(),
		Params:      op.Params,
		Mutating:    op.Mutating(),
	}
}

// Kinds returns the catalog kinds (sorted by name) for the cascade picker.
func Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(resCatalog.Kinds))
	for _, k := range resCatalog.Kinds {
		out = append(out, KindInfo{Kind: k.Kind, Capabilities: k.capabilities(), OpCount: len(k.Operations)})
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

// OpCard is one search candidate carrying everything needed to execute it in
// the same turn: the structural identity (kind/operationId/method/path), the
// operation's summary, whether it writes, and its input surface (params with
// descriptions + body skeleton). Cards exist because the ranking corpus reads
// the summaries but a thin result hid them from the model — the model was
// re-ranking blind on six structural fields (design doc §3.2).
type OpCard struct {
	Kind        string           `json:"kind"`
	OperationID string           `json:"operationId"`
	Method      string           `json:"method"`
	Path        string           `json:"path"`
	Summary     string           `json:"summary,omitempty"`
	Write       bool             `json:"write"`
	Params      []DistilledParam `json:"params,omitempty"`
	Body        []DistilledField `json:"body,omitempty"`
}

// ThinOp is a tail candidate beyond the card window: enough to recognize a
// missed target and follow up (describe the kind, or refine the query), at
// 4 fields instead of a full card.
type ThinOp struct {
	Kind        string `json:"kind"`
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
}

// SearchResult is the two-segment search answer: full executable cards for the
// top candidates, thin entries for the rest of the window. The tail exists
// because the scorer is substring/token-overlap and the right op can rank
// 9-15 on a fuzzy query (baseline: top-20 recall is +6pp over top-5); if a
// future eval shows top-K recall ≈ top-20 recall, delete the tail.
type SearchResult struct {
	Cards []OpCard `json:"cards"`
	More  []ThinOp `json:"more,omitempty"`
}

// searchCardK / searchCardKMax bound the card window. K=5 comes from the
// eval baseline: ranks 6-8 had zero additional hits, so larger
// windows pay card bytes for no recall.
const (
	searchCardK    = 5
	searchCardKMax = 8
)

// SearchCards ranks operations against a free-text query and returns the
// two-segment result: the top cardK candidates as executable cards (cardK<=0
// defaults to 5, capped at 8), the remainder up to limit (default 20) as thin
// entries. Cards are assembled from the init-time DistilledOp memo — no spec
// is re-read or re-parsed on the query path.
func SearchCards(query string, cardK, limit int) SearchResult {
	if cardK <= 0 {
		cardK = searchCardK
	}
	if cardK > searchCardKMax {
		cardK = searchCardKMax
	}
	if limit <= 0 {
		limit = 20
	}
	if limit < cardK {
		limit = cardK
	}
	cands := Search(query, limit)
	res := SearchResult{Cards: make([]OpCard, 0, cardK)} // empty array (not null) on no match
	for i, op := range cands {
		if i < cardK {
			dop := distilledIdx[[2]string{op.Kind, op.OperationID}]
			res.Cards = append(res.Cards, OpCard{
				Kind:        op.Kind,
				OperationID: op.OperationID,
				Method:      op.Method,
				Path:        op.Path,
				Summary:     dop.Summary,
				Write:       op.Mutating(),
				Params:      dop.Params,
				Body:        dop.Body,
			})
			continue
		}
		res.More = append(res.More, ThinOp{
			Kind: op.Kind, OperationID: op.OperationID, Method: op.Method, Path: op.Path,
		})
	}
	return res
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
