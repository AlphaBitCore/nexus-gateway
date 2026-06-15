// Package resource: minimal OpenAPI document shapes (only the fields the
// distiller reads) and same-document $ref resolution.
package resource

import "strings"

type oapiDoc struct {
	Paths      map[string]map[string]oapiOp `yaml:"paths"`
	Components oapiComponents               `yaml:"components"`
}

type oapiComponents struct {
	Schemas map[string]oapiSchema `yaml:"schemas"`
}

type oapiOp struct {
	Summary     string                  `yaml:"summary"`
	Description string                  `yaml:"description"`
	Parameters  []oapiParam             `yaml:"parameters"`
	RequestBody *oapiReqBody            `yaml:"requestBody"`
	Responses   map[string]oapiResponse `yaml:"responses"`
}

type oapiResponse struct {
	Content map[string]struct {
		Schema oapiSchema `yaml:"schema"`
	} `yaml:"content"`
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
	Items      *oapiSchema           `yaml:"items"`    // array item schema (response-tree recursion)
	Nullable   bool                  `yaml:"nullable"` // OpenAPI 3.0-style nullability
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
