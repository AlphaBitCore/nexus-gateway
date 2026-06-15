package resource

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// distill.go turns a kind's full OpenAPI spec into a compact, model-facing schema.
// resource_describe used to return the entire OpenAPI YAML (several KB that then
// persist as a tool result in the agent's context); the model only needs, per
// operation, the verb + path + parameters + request-body fields. Distilling cuts
// that tool result ~5-10× and keeps the context small.

// DistilledKind is the compact schema Distill (and the resource_describe tool)
// returns.
type DistilledKind struct {
	Kind       string        `json:"kind"`
	BasePrefix string        `json:"basePrefix"`
	Operations []DistilledOp `json:"operations"`
}

type DistilledOp struct {
	OperationID string           `json:"operationId"`
	Verb        string           `json:"verb,omitempty"`    // canonical CRUD/action verb, or "" for a non-CRUD op
	Label       string           `json:"label"`             // short human/agent name (verb or path tail)
	Summary     string           `json:"summary,omitempty"` // the operation's OpenAPI summary/description — what it does
	Method      string           `json:"method"`
	Path        string           `json:"path"`
	Params      []DistilledParam `json:"params,omitempty"`
	Body        []DistilledField `json:"body,omitempty"`

	// searchText is the summary PLUS the longer description, lowercased, used only to
	// build the search corpus (it is unexported so it never bloats the model-facing
	// JSON). The model-facing Summary stays concise (summary-preferred); search,
	// however, indexes both — so a query phrased in the words of the description (e.g.
	// "pipeline" for an op whose summary is "Get hook execution chain" but whose
	// description mentions the pipeline visualiser) still finds the op.
	searchText string
}

type DistilledParam struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Type     string `json:"type,omitempty"`
	Desc     string `json:"desc,omitempty"` // what this param does — so the model picks the right filter
	Enum     []any  `json:"enum,omitempty"`
}

type DistilledField struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required"`
	Enum     []any  `json:"enum,omitempty"`
	// Nullable marks a field whose op contract allows null (a 3.1 type union
	// ["x","null"] or 3.0 nullable:true) — accurate metadata for the model:
	// such a field can come back null in real rows.
	Nullable bool `json:"nullable,omitempty"`
}

// Distill returns the compact, model-facing schema for a kind by name — every
// operation with its operationId/verb/label/method/path, params, and body fields —
// from the embedded OpenAPI spec. ok is false for an unknown kind. For an embedded
// kind the spec always reads and parses (guarded by TestResourceEmbedConsistent), so
// a spec read/parse failure there also yields ok=false rather than a surfaced error;
// the only reachable false in practice is an unknown kind.
func Distill(kind string) (DistilledKind, bool) {
	rk, ok := resIdx[strings.TrimSpace(kind)]
	if !ok {
		return DistilledKind{}, false
	}
	raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + rk.File)
	if err != nil {
		return DistilledKind{}, false
	}
	d, err := distillKind(rk, raw)
	if err != nil {
		return DistilledKind{}, false
	}
	return d, true
}

// distillKind parses a kind's OpenAPI YAML and returns the compact schema. It is
// pure (no embed) so it is unit-testable against fixture bytes.
func distillKind(rk resourceKind, raw []byte) (DistilledKind, error) {
	var doc oapiDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return DistilledKind{}, fmt.Errorf("parse %s spec: %w", rk.Kind, err)
	}
	out := DistilledKind{Kind: rk.Kind, BasePrefix: resCatalog.BasePrefix}
	// Walk every catalog operation (the authoritative method/path set) and enrich
	// each from the parsed spec. EVERY operation is emitted — reports, singleton
	// config, RPCs, and nested sub-resources included — so the model can reach the
	// whole surface, not just the canonical CRUD shapes.
	for _, raw := range rk.Operations {
		op := rk.operation(raw)
		d := DistilledOp{
			OperationID: op.OperationID,
			Verb:        op.CanonicalVerb(),
			Label:       op.Label(),
			Method:      op.Method,
			Path:        op.Path,
		}
		methods := doc.Paths[op.Path]
		spec, ok := methods[strings.ToLower(op.Method)]
		if ok {
			d.Summary = strings.TrimSpace(spec.Summary)
			if d.Summary == "" {
				d.Summary = strings.TrimSpace(spec.Description)
			}
			// The search corpus indexes summary AND description (the model-facing
			// Summary stays summary-preferred), so a query in the description's words
			// still finds the op.
			d.searchText = strings.ToLower(strings.TrimSpace(spec.Summary + " " + spec.Description))
			for _, p := range spec.Parameters {
				ps := resolveSchema(p.Schema, &doc, map[string]bool{})
				d.Params = append(d.Params, DistilledParam{
					Name: p.Name, In: p.In, Required: p.Required,
					Type: typeStr(ps.Type), Desc: strings.TrimSpace(p.Description), Enum: ps.Enum,
				})
			}
			d.Body = distillBody(spec.RequestBody, &doc)
		}
		out.Operations = append(out.Operations, d)
	}
	return out, nil
}

// distillBody extracts the JSON request body's fields (name/type/required/enum),
// resolving same-document $refs at both the body root and each property (37 of
// the catalog's 117 JSON bodies are a root $ref — without resolution they
// distill to nothing and a "directly executable" search card ships no skeleton).
func distillBody(rb *oapiReqBody, doc *oapiDoc) []DistilledField {
	if rb == nil {
		return nil
	}
	media, ok := rb.Content["application/json"]
	if !ok {
		return nil
	}
	schema := resolveSchema(media.Schema, doc, map[string]bool{})
	// Body fields stay FLAT — the model-facing skeleton lists the top-level
	// request fields only.
	return fieldsOf(schema, doc)
}

// fieldsOf distills one resolved schema level into flat fields.
func fieldsOf(schema oapiSchema, doc *oapiDoc) []DistilledField {
	if len(schema.Properties) == 0 {
		return nil
	}
	req := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		req[r] = true
	}
	fields := make([]DistilledField, 0, len(schema.Properties))
	for name, prop := range schema.Properties {
		prop = resolveSchema(prop, doc, map[string]bool{})
		typ, nullable := typeInfo(prop.Type)
		fields = append(fields, DistilledField{
			Name: name, Type: typ, Required: req[name], Enum: prop.Enum,
			Nullable: nullable || prop.Nullable,
		})
	}
	sortFields(fields)
	return fields
}

// typeStr normalizes an OpenAPI `type` (a string, or a 3.1 union like
// [string,null]) to a compact string; the "null" member is dropped.
func typeStr(t any) string {
	s, _ := typeInfo(t)
	return s
}

// typeInfo normalizes the type AND reports whether the union carried a "null"
// member (the field's nullability bit).
func typeInfo(t any) (string, bool) {
	switch v := t.(type) {
	case string:
		return v, false
	case []any:
		var parts []string
		nullable := false
		for _, e := range v {
			if s, ok := e.(string); ok {
				if s == "null" {
					nullable = true
					continue
				}
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "|"), nullable
	default:
		return "", false
	}
}

// sortFields orders body fields required-first, then by name, so the model reads
// the must-supply fields up top deterministically.
func sortFields(f []DistilledField) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Required != f[j].Required {
			return f[i].Required // required first
		}
		return f[i].Name < f[j].Name
	})
}
