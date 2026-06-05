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
}

// --- minimal OpenAPI shapes (only the fields the distiller reads) ---

type oapiDoc struct {
	Paths      map[string]map[string]oapiOp `yaml:"paths"`
	Components oapiComponents               `yaml:"components"`
}

type oapiComponents struct {
	Schemas map[string]oapiSchema `yaml:"schemas"`
}

type oapiOp struct {
	Summary     string       `yaml:"summary"`
	Description string       `yaml:"description"`
	Parameters  []oapiParam  `yaml:"parameters"`
	RequestBody *oapiReqBody `yaml:"requestBody"`
}

type oapiParam struct {
	Name        string     `yaml:"name"`
	In          string     `yaml:"in"`
	Required    bool       `yaml:"required"`
	Description string     `yaml:"description"`
	Schema      oapiSchema `yaml:"schema"`
}

type oapiReqBody struct {
	Content map[string]struct {
		Schema oapiSchema `yaml:"schema"`
	} `yaml:"content"`
}

type oapiSchema struct {
	Ref        string                `yaml:"$ref"` // same-document ref: #/components/schemas/<Name>
	Type       any                   `yaml:"type"` // string, or []any for 3.1 unions ([string,null])
	Enum       []any                 `yaml:"enum"`
	Properties map[string]oapiSchema `yaml:"properties"`
	Required   []string              `yaml:"required"`
}

// refMaxHops caps $ref chain following. The embedded corpus's deepest real
// chain is 3 hops (the device-groups membership-query bodies), and every ref
// is same-document (#/components/schemas/...) — measured 2026-06-05, guarded
// by TestDistillRefBodiesNonEmpty. The cap is inclusive: a 3-hop chain resolves.
const refMaxHops = 3

// resolveSchema follows a schema's $ref chain through the document's
// components/schemas, up to refMaxHops hops, with a visited set so a cyclic
// ref terminates. An unresolvable ref (unknown name, non-local ref, cycle,
// or a chain deeper than the cap) yields the zero schema — the body then
// distills empty, exactly the pre-resolution behavior.
func resolveSchema(s oapiSchema, doc *oapiDoc, seen map[string]bool) oapiSchema {
	for hops := 0; s.Ref != "" && hops < refMaxHops; hops++ {
		name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/")
		if !ok || seen[name] {
			return oapiSchema{}
		}
		seen[name] = true
		next, ok := doc.Components.Schemas[name]
		if !ok {
			return oapiSchema{}
		}
		s = next
	}
	if s.Ref != "" { // chain deeper than the cap
		return oapiSchema{}
	}
	return s
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
	req := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		req[r] = true
	}
	fields := make([]DistilledField, 0, len(schema.Properties))
	for name, prop := range schema.Properties {
		prop = resolveSchema(prop, doc, map[string]bool{})
		fields = append(fields, DistilledField{
			Name: name, Type: typeStr(prop.Type), Required: req[name], Enum: prop.Enum,
		})
	}
	sortFields(fields)
	return fields
}

// typeStr normalizes an OpenAPI `type` (a string, or a 3.1 union like
// [string,null]) to a compact string; the "null" member is dropped.
func typeStr(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, e := range v {
			if s, ok := e.(string); ok && s != "null" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "|")
	default:
		return ""
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
