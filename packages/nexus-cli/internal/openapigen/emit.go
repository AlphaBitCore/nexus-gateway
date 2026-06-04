package openapigen

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// methodOrder fixes the serialisation order of HTTP methods within a path item.
var methodOrder = map[string]int{"get": 0, "post": 1, "put": 2, "patch": 3, "delete": 4}

// document is one kind's assembled OpenAPI spec plus the routes that built it.
type document struct {
	Kind   string
	Doc    *omap
	Routes []route // sorted, for the index catalog
}

// buildDocuments groups routes by resource kind and assembles one OpenAPI 3.1
// document per kind. Routes whose kind cannot be derived are reported.
func buildDocuments(routes []route, opts Options, rep *Report) []document {
	byKind := map[string][]route{}
	for _, r := range routes {
		kind := deriveKind(r.Path, opts.BasePrefix)
		if kind == "" {
			rep.addUnresolved("route %s %s has no derivable resource kind", r.Method, r.Path)
			continue
		}
		byKind[kind] = append(byKind[kind], r)
	}

	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var docs []document
	for _, kind := range kinds {
		krs := byKind[kind]
		sortRoutes(krs)
		docs = append(docs, document{Kind: kind, Doc: buildDoc(kind, krs, opts), Routes: krs})
	}
	return docs
}

// buildDoc assembles the OpenAPI document for one kind.
func buildDoc(kind string, routes []route, opts Options) *omap {
	sb := newSchemaBuilder()

	paths := newOMap()
	for _, r := range routes {
		oapiPath, params := echoToOpenAPIPath(r.Path)
		item, ok := paths.Get(oapiPath)
		var pathItem *omap
		if ok {
			pathItem = item.(*omap)
		} else {
			pathItem = newOMap()
			paths.Set(oapiPath, pathItem)
		}
		pathItem.Set(strings.ToLower(r.Method), buildOperation(kind, r, params, sb))
	}

	doc := newOMap()
	doc.Set("openapi", "3.1.0")
	doc.Set("info", newOMap().
		Set("title", opts.Title+" — "+kind).
		Set("version", opts.Version).
		Set("description", "Generated from control-plane source by openapigen. Structural draft: field names, types, optionality, status codes and IAM tiers are code-derived; enum value sets and required-field rules enforced imperatively in handlers are filled in by the openapi-review skill."))
	doc.Set("paths", paths)

	if names := sb.componentNames(); len(names) > 0 {
		schemas := newOMap()
		for _, n := range names {
			schemas.Set(n, sb.components[n])
		}
		doc.Set("components", newOMap().Set("schemas", schemas))
	}
	return doc
}

// buildOperation assembles a single OpenAPI operation object.
func buildOperation(kind string, r route, params []string, sb *schemaBuilder) *omap {
	op := newOMap()
	op.Set("operationId", r.OperationID)
	op.Set("summary", generatedSummary(r.Method, kind))
	op.SetIf(r.IAMAction != "", "x-nexus-iam-action", r.IAMAction)
	op.Set("x-nexus-tier", string(r.Tier))

	if len(params) > 0 || len(r.QueryParams) > 0 {
		var ps []any
		for _, p := range params {
			ps = append(ps, newOMap().
				Set("name", p).
				Set("in", "path").
				Set("required", true).
				Set("schema", newOMap().Set("type", "string")))
		}
		for _, q := range r.QueryParams {
			ps = append(ps, newOMap().
				Set("name", q).
				Set("in", "query").
				Set("required", false).
				Set("schema", newOMap().Set("type", "string")))
		}
		op.Set("parameters", ps)
	}

	if r.Request != nil && (r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH") {
		op.Set("requestBody", newOMap().
			Set("required", true).
			Set("content", newOMap().Set("application/json",
				newOMap().Set("schema", sb.schemaFor(r.Request)))))
	}

	op.Set("responses", buildResponses(r, sb))
	return op
}

// buildResponses renders the response object for a route. When no c.JSON body
// type was recovered, a minimal description-only response is emitted so the
// spec stays valid for the audit skill to enrich.
func buildResponses(r route, sb *schemaBuilder) *omap {
	resps := newOMap()
	for _, resp := range r.Responses {
		key := "default"
		if resp.Status > 0 {
			key = strconv.Itoa(resp.Status)
		}
		body := newOMap().Set("description", statusText(resp.Status))
		if resp.Type != nil {
			body.Set("content", newOMap().Set("application/json",
				newOMap().Set("schema", sb.schemaFor(resp.Type))))
		}
		resps.Set(key, body)
	}
	if resps.Len() == 0 {
		resps.Set("default", newOMap().Set("description", "Response body not statically resolved; see openapi-review."))
	}
	return resps
}

func generatedSummary(method, kind string) string {
	verb := map[string]string{
		"GET": "Read", "POST": "Create", "PUT": "Update", "PATCH": "Update", "DELETE": "Delete",
	}[method]
	if verb == "" {
		verb = method
	}
	return verb + " " + kind
}

func statusText(code int) string {
	if code == 0 {
		return "Unresolved status"
	}
	if t := http.StatusText(code); t != "" {
		return t
	}
	return strconv.Itoa(code)
}

// sortRoutes orders routes deterministically by path then method.
func sortRoutes(rs []route) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Path != rs[j].Path {
			return rs[i].Path < rs[j].Path
		}
		return methodOrder[strings.ToLower(rs[i].Method)] < methodOrder[strings.ToLower(rs[j].Method)]
	})
}
