package codec

// Gemini's function-declaration `parameters` and generationConfig
// `responseSchema` are proto-backed Schema objects, not free-form JSON
// Schema: any key outside the proto's field set fails the WHOLE request
// with HTTP 400:
//
//	Invalid JSON payload received. Unknown name "additionalProperties" at
//	'tools[0].function_declarations[0].parameters': Cannot find field.
//
// Observed upstream for `additionalProperties` and `$comment`, and
// confirmed keyword-by-keyword against the live API on both the tools and
// responseSchema paths (identical accept/reject sets).
//
// geminiSchemaAllowedKeys is the COMPLETE Schema field set from the API's
// authoritative machine-readable definition — `.schemas.Schema.properties`
// in the v1beta discovery document
// (https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta),
// 22 fields — plus `oneOf`/`allOf`, which the discovery document omits but
// the live API accepts (probed 200; an unknown name would 400, so the
// serving proto must carry them). Refresh against that discovery URL when
// Google evolves the Schema proto. `example` (singular) is accepted while
// `examples` (plural) is rejected.
var geminiSchemaAllowedKeys = map[string]struct{}{
	"type":             {},
	"format":           {},
	"title":            {},
	"description":      {},
	"nullable":         {},
	"enum":             {},
	"pattern":          {},
	"default":          {},
	"example":          {},
	"items":            {},
	"minItems":         {},
	"maxItems":         {},
	"properties":       {},
	"required":         {},
	"minProperties":    {},
	"maxProperties":    {},
	"propertyOrdering": {},
	"minLength":        {},
	"maxLength":        {},
	"minimum":          {},
	"maximum":          {},
	"anyOf":            {},
	"oneOf":            {},
	"allOf":            {},
}

// sanitizeGeminiSchema recursively rewrites a caller-supplied JSON-Schema
// node into the subset Gemini's proto Schema accepts. Keys outside the
// accept set are dropped rather than forwarded — a dropped constraint is a
// weaker hint to the model, while a forwarded unknown key rejects the whole
// request. Where a rejected keyword has a cheap semantic equivalent it is
// converted instead of dropped:
//
//	type: ["T","null"]        → type: "T" + nullable: true (proto type is not repeated)
//	const: v                  → enum: [v]
//	examples: [a, ...]        → example: a
//	exclusiveMinimum: n       → minimum: n (numeric form; closest inclusive hint)
//	exclusiveMaximum: n       → maximum: n
//
// `$ref`/`$defs` have no cheap equivalent (the proto has no reference
// mechanism) and are dropped.
func sanitizeGeminiSchema(v any) any {
	node, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(node))
	nullableFromType := false
	for k, val := range node {
		switch k {
		case "properties":
			if props, ok := val.(map[string]any); ok {
				cleaned := make(map[string]any, len(props))
				for name, sub := range props {
					cleaned[name] = sanitizeGeminiSchema(sub)
				}
				out["properties"] = cleaned
			}
		case "items":
			// JSON-Schema tuple form (array of schemas) has no proto
			// equivalent (items is a single Schema); keep the first
			// element's shape rather than losing the item type entirely.
			if arr, ok := val.([]any); ok {
				if len(arr) > 0 {
					out["items"] = sanitizeGeminiSchema(arr[0])
				}
			} else {
				out["items"] = sanitizeGeminiSchema(val)
			}
		case "anyOf", "oneOf", "allOf":
			if arr, ok := val.([]any); ok {
				cleaned := make([]any, 0, len(arr))
				for _, sub := range arr {
					cleaned = append(cleaned, sanitizeGeminiSchema(sub))
				}
				out[k] = cleaned
			}
		case "type":
			// Union form ["string","null"] fails with `Proto field is not
			// repeating, cannot start list` — collapse to the first
			// non-null entry and carry nullability separately.
			if arr, ok := val.([]any); ok {
				for _, t := range arr {
					s, _ := t.(string)
					if s == "null" {
						nullableFromType = true
						continue
					}
					if _, has := out["type"]; !has && s != "" {
						out["type"] = s
					}
				}
			} else {
				out["type"] = val
			}
		case "enum":
			// Gemini's Schema proto rejects an empty-string entry in an enum
			// array with HTTP 400 "...enum[N]: cannot be empty" (observed on
			// gemini-2.5-pro, 2026-07-17 prod, from OpenAPI-generated tool
			// schemas that emit "" as an unset/default sentinel). Drop the
			// empty entries; the remaining values still constrain the model.
			// If nothing survives, drop the enum key entirely — an empty enum
			// is itself rejected, and no constraint is better than a rejected
			// request. A non-array enum (malformed) is forwarded for the
			// upstream to judge.
			if arr, ok := val.([]any); ok {
				cleaned := make([]any, 0, len(arr))
				for _, e := range arr {
					if s, isStr := e.(string); isStr && s == "" {
						continue
					}
					cleaned = append(cleaned, e)
				}
				if len(cleaned) > 0 {
					out["enum"] = cleaned
				}
			} else {
				out["enum"] = val
			}
		case "const":
			if _, has := node["enum"]; !has {
				out["enum"] = []any{val}
			}
		case "examples":
			if _, has := node["example"]; !has {
				if arr, ok := val.([]any); ok && len(arr) > 0 {
					out["example"] = arr[0]
				}
			}
		case "exclusiveMinimum":
			// Numeric (draft-2020) form only; the boolean (draft-4) form
			// modifies `minimum`, which is already kept.
			if f, ok := val.(float64); ok {
				if _, has := node["minimum"]; !has {
					out["minimum"] = f
				}
			}
		case "exclusiveMaximum":
			if f, ok := val.(float64); ok {
				if _, has := node["maximum"]; !has {
					out["maximum"] = f
				}
			}
		default:
			if _, allowed := geminiSchemaAllowedKeys[k]; allowed {
				out[k] = val
			}
		}
	}
	if nullableFromType {
		out["nullable"] = true
	}
	return out
}
