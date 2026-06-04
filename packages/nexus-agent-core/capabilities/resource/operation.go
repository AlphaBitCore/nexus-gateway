package resource

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// operation.go is the generic, OpenAPI-driven operation engine. It treats the
// embedded catalog as a flat list of (method, path) operations and derives
// everything an agent or the TUI needs to invoke ANY of them — the ordered path
// parameters, a stable canonical verb where one applies, a friendly label
// otherwise — with NO per-kind special-casing. The generality is the point: it is
// the path by which a traditional REST API, described only by its OpenAPI spec,
// becomes uniformly AI-callable and deterministically navigable, at any nesting
// depth. Discovery is code-level (Search ranks candidates) so the model
// picks from a small set instead of being handed the whole catalog.

// Operation is one catalog operation, classified for generic invocation.
type Operation struct {
	Kind        string
	Method      string // GET/POST/PUT/PATCH/DELETE (upper-cased)
	Path        string // e.g. /api/admin/nodes/{id}/overrides/{configKey}
	OperationID string
	Tier        string
	IAMAction   string
	Params      []string // ordered path-parameter names: ["id","configKey"]
}

// Mutating reports whether the operation changes server state (anything but GET).
// The read tools refuse a mutating op; the write tool/confirm gate guards it.
func (o Operation) Mutating() bool { return !strings.EqualFold(o.Method, "GET") }

// CanonicalVerb returns the CRUD/action verb for an operation when its path
// matches a canonical shape relative to its kind's collection, else "". It is a
// convenience label (list/get/create/update/delete/action:<x>), not a filter —
// every operation is reachable regardless of whether it has a canonical verb.
func (o Operation) CanonicalVerb() string {
	coll := collectionPath(o.Kind)
	m := strings.ToUpper(o.Method)
	if o.Path == coll {
		switch m {
		case "GET":
			return "list"
		case "POST":
			return "create"
		}
		return ""
	}
	rest, ok := strings.CutPrefix(o.Path, coll+"/")
	if !ok {
		return ""
	}
	if isSingleParamSeg(rest) {
		switch m {
		case "GET":
			return "get"
		case "PUT", "PATCH":
			return "update"
		case "DELETE":
			return "delete"
		}
		return ""
	}
	if seg := strings.Split(rest, "/"); len(seg) == 2 && isSingleParamSeg(seg[0]) && m == "POST" {
		return "action:" + seg[1]
	}
	return ""
}

// Label is a short, human/agent-facing name for the operation: the canonical verb
// when one applies, otherwise the path tail beyond the kind's collection (so
// report/RPC/nested ops read as "config", "summary", "provider/{providerId}",
// "{id}/runs", …), with the method prefixed for writes so they are visibly
// distinct from reads.
func (o Operation) Label() string {
	if v := o.CanonicalVerb(); v != "" {
		return v
	}
	coll := collectionPath(o.Kind)
	tail := strings.TrimPrefix(o.Path, coll+"/")
	if tail == o.Path || tail == "" { // not under the collection (or is the collection itself)
		tail = strings.TrimPrefix(o.Path, strings.TrimRight(resCatalog.BasePrefix, "/")+"/")
	}
	if tail == "" || tail == o.Path {
		tail = strings.ToLower(o.Method)
	}
	if o.Mutating() {
		return strings.ToLower(o.Method) + " " + tail
	}
	return tail
}

// pathParams returns the ordered {name} parameters in a path. Params are matched
// anywhere (not just whole segments) so the engine generalizes to OpenAPI paths
// like /a/{id}.json, though the CP catalog uses whole-segment params.
func pathParams(path string) []string {
	var out []string
	rest := path
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			break
		}
		out = append(out, rest[i+1:i+j])
		rest = rest[i+j+1:]
	}
	return out
}

// SubstituteParams fills every {name} in path with url.PathEscape(vals[name]). It
// errors if any parameter has no value, so a half-substituted path never reaches
// the server — the subFirstParam predecessor substituted only the FIRST brace,
// silently leaving any 2nd {param} verbatim and 404-ing every deep path. Unknown
// keys in vals are ignored; an unbalanced "{" is emitted verbatim.
func SubstituteParams(path string, vals map[string]string) (string, error) {
	var b strings.Builder
	var missing []string
	rest := path
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			b.WriteString(rest) // unbalanced brace — leave as-is
			break
		}
		name := rest[i+1 : i+j]
		b.WriteString(rest[:i])
		v, ok := vals[name]
		if !ok || v == "" {
			missing = append(missing, name)
		}
		b.WriteString(url.PathEscape(v))
		rest = rest[i+j+1:]
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("missing path parameter(s): %s", strings.Join(missing, ", "))
	}
	return b.String(), nil
}

// operation builds an Operation (with ordered path params) from a raw catalog op.
func (rk resourceKind) operation(raw resourceOp) Operation {
	return Operation{
		Kind:        rk.Kind,
		Method:      strings.ToUpper(raw.Method),
		Path:        raw.Path,
		OperationID: raw.OperationID,
		Tier:        raw.Tier,
		IAMAction:   raw.IAMAction,
		Params:      pathParams(raw.Path),
	}
}

// operations returns every catalog operation for the kind, in catalog order.
func (rk resourceKind) operations() []Operation {
	out := make([]Operation, 0, len(rk.Operations))
	for _, raw := range rk.Operations {
		out = append(out, rk.operation(raw))
	}
	return out
}

// canonicalVerbs lists the canonical CRUD/action verbs the kind supports, derived
// from its operations — a quick hint for a picker. It is intentionally a subset:
// a non-CRUD kind (reports/singleton config/RPC) returns few or none, but every
// such operation is still reachable via operations() / resource_search.
func (rk resourceKind) canonicalVerbs() []string {
	var v []string
	for _, op := range rk.operations() {
		if cv := op.CanonicalVerb(); cv != "" {
			v = append(v, cv)
		}
	}
	return v
}

// FindOp resolves a (kind, operationId) pair to its Operation. operationId is the
// catalog-stable identifier the model passes to resource_read / resource_invoke.
func FindOp(kind, operationID string) (Operation, bool) {
	rk, ok := resIdx[strings.TrimSpace(kind)]
	if !ok {
		return Operation{}, false
	}
	for _, raw := range rk.Operations {
		if raw.OperationID == operationID {
			return rk.operation(raw), true
		}
	}
	return Operation{}, false
}

// opSearchEntry is one catalog operation paired with its precomputed lowercased
// "extra" search text — the operation's summary plus its query-param names and
// descriptions, which the structural kind/operationId/path/label scoring does not
// otherwise see. Built once into opIndex so search is one pass over the catalog per
// call with no per-call distill, and so a question phrased in the words of a filter
// or of the operation's purpose still finds the op.
type opSearchEntry struct {
	op    Operation
	extra string
}

// opIndex is the memoized search corpus. It is built by buildOpIndex from catalog.go's
// init (after the catalog is parsed) so the search path never re-distills the specs.
var opIndex []opSearchEntry

// buildOpIndex distills every kind once and pairs each operation with its extra
// search text. A kind whose spec fails to distill still contributes its operations
// with empty extra text (best-effort: the structural kind/operationId/path/label
// scoring still applies), so search never depends on a spec parsing cleanly.
func buildOpIndex() []opSearchEntry {
	out := make([]opSearchEntry, 0, 512)
	for _, k := range resCatalog.Kinds {
		var byID map[string]DistilledOp
		if raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + k.File); err == nil {
			if d, err := distillKind(k, raw); err == nil {
				byID = make(map[string]DistilledOp, len(d.Operations))
				for _, dop := range d.Operations {
					byID[dop.OperationID] = dop
				}
			}
		}
		for _, op := range k.operations() {
			out = append(out, opSearchEntry{op: op, extra: opExtraCorpus(byID[op.OperationID])})
		}
	}
	return out
}

// opExtraCorpus is the lowercased summary + query-param names/descriptions for an
// operation — the words an operator is likely to use that the path/operationId do
// not contain (e.g. a `provider` filter, or a summary like "list blocked requests").
func opExtraCorpus(d DistilledOp) string {
	var b strings.Builder
	// Prefer the fuller search text (summary + description); fall back to the
	// summary for a directly-constructed DistilledOp (e.g. a unit test).
	if d.searchText != "" {
		b.WriteString(d.searchText)
	} else {
		b.WriteString(strings.ToLower(d.Summary))
	}
	for _, p := range d.Params {
		if p.In != "query" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(p.Name))
		if p.Desc != "" {
			b.WriteByte(' ')
			b.WriteString(strings.ToLower(p.Desc))
		}
	}
	return strings.TrimSpace(b.String())
}

// Search ranks catalog operations against a free-text query using code-level
// matching (no LLM): token overlap and substring hits over the kind name,
// operationId, label, path, and the op's summary + query-param corpus. It returns
// the top `limit` candidates so a caller hands the model a short, relevant list
// instead of all 364 operations — the grep-first discovery pattern that keeps token
// cost flat as the catalog (or an arbitrary external API) grows. A blank query
// returns the first `limit` operations in catalog order (a stable browse).
func Search(query string, limit int) []Operation {
	if limit <= 0 {
		limit = 20
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		out := make([]Operation, 0, limit)
		for i := 0; i < len(opIndex) && i < limit; i++ {
			out = append(out, opIndex[i].op)
		}
		return out
	}
	terms := tokenize(q)
	type scored struct {
		op    Operation
		score int
		idx   int
	}
	ranked := make([]scored, 0, len(opIndex))
	for i, e := range opIndex {
		if s := scoreOperation(e.op, e.extra, q, terms); s > 0 {
			ranked = append(ranked, scored{op: e.op, score: s, idx: i})
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].idx < ranked[b].idx // stable: catalog order breaks ties
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Operation, len(ranked))
	for i := range ranked {
		out[i] = ranked[i].op
	}
	return out
}

// scoreOperation scores one operation against the lowercased query and its tokens.
// Whole-query substring hits on kind/operationId/path rank highest; per-token hits
// add up so a multi-word query ("node override") still finds a path that contains
// both. `extra` is the op's summary + query-param names/descriptions (precomputed in
// opIndex): it matches at a lower weight than the structural fields, so a question
// phrased in the words of a filter ("filter traffic by provider") or of the
// operation's purpose ("why was a request blocked") still surfaces the right op.
// Zero means no match (the op is dropped from the candidate list).
func scoreOperation(op Operation, extra string, q string, terms []string) int {
	kind := strings.ToLower(op.Kind)
	opID := strings.ToLower(op.OperationID)
	path := strings.ToLower(op.Path)
	label := strings.ToLower(op.Label())
	score := 0
	switch {
	case kind == q:
		score += 100
	case strings.HasPrefix(kind, q):
		score += 40
	case strings.Contains(kind, q):
		score += 20
	}
	if strings.Contains(opID, q) {
		score += 30
	}
	if strings.Contains(label, q) {
		score += 15
	}
	if strings.Contains(path, q) {
		score += 10
	}
	if extra != "" && strings.Contains(extra, q) {
		score += 8
	}
	for _, t := range terms {
		switch {
		case kind == t:
			score += 12
		case strings.Contains(kind, t):
			score += 6
		}
		if strings.Contains(opID, t) {
			score += 5
		}
		if strings.Contains(label, t) {
			score += 4
		}
		if strings.Contains(path, t) {
			score += 3
		}
		if extra != "" && strings.Contains(extra, t) {
			score += 2
		}
	}
	return score
}

// tokenize splits a query into lowercase word tokens on whitespace and the path
// separators (/ - _ .) so "node/override" and "node override" match alike.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '/' || r == '-' || r == '_' || r == '.'
	})
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
