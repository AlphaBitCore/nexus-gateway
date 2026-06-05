package resource

import (
	"sort"
	"strings"
)

// capability.go derives the human-facing capability profile of a kind — the
// CAPABILITIES column of `nexus resource kinds` and the TUI picker line. It is
// a TOTAL mapping (design doc §3.3): four rules, evaluated in order, partition
// every operation, so an empty profile is impossible by construction. The
// profile is a display hint ONLY — the AI consumes summary + method + path and
// never reads it — which is why a load-time heuristic is acceptable here while
// the model-facing fields are pass-through from the spec.
//
// CanonicalVerb()/Label() are untouched: they feed search scoring and the
// model-facing DistilledOp; this is a separate display pass.

// crudVerbs is the full canonical set that collapses to the single token "crud".
var crudVerbs = [...]string{"list", "get", "create", "update", "delete"}

// capabilities returns the deduplicated capability profile for the kind:
//
//	rule 1: canonical CRUD shapes keep their CanonicalVerb (list/get/create/
//	        update/delete/action:<x>).
//	rule 2: a GET + (PUT|PATCH) pair on the SAME path is singleton config —
//	        including the collection root, where the pair ABSORBS the
//	        canonical `list` the root GET would otherwise claim (a kind whose
//	        root is read+replace is a config surface, not a collection).
//	rule 3: any remaining GET is a report (param-bearing tails included).
//	rule 4: any remaining non-GET is action:<seg>, <seg> = last non-{param}
//	        path segment (collection-level RPCs like /routing-rules/simulate).
//
// Output order: crud (or the partial CRUD verbs), config, report, then
// action:* sorted — deterministic for tables and snapshots.
func (rk resourceKind) capabilities() []string {
	coll := collectionPath(rk.Kind)
	ops := rk.operations()

	// Rule 2 pairing: paths that expose BOTH a GET and a PUT/PATCH.
	hasGet := map[string]bool{}
	hasPut := map[string]bool{}
	for _, op := range ops {
		switch op.Method {
		case "GET":
			hasGet[op.Path] = true
		case "PUT", "PATCH":
			hasPut[op.Path] = true
		}
	}
	isConfigPath := func(p string) bool { return hasGet[p] && hasPut[p] }

	seen := map[string]bool{}
	var crud, rest []string
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		for _, c := range crudVerbs {
			if v == c {
				crud = append(crud, v)
				return
			}
		}
		rest = append(rest, v)
	}

	for _, op := range ops {
		// Rule 2 first where it overrides rule 1: the root pair absorbs `list`,
		// and a canonical {id} GET+PUT pair stays get/update (rule 1) because
		// an item path is collection access, not a singleton config surface.
		if isConfigPath(op.Path) && (op.Path == coll || op.CanonicalVerb() == "") {
			switch op.Method {
			case "GET", "PUT", "PATCH":
				add("config")
				continue
			}
		}
		if v := op.CanonicalVerb(); v != "" { // rule 1
			add(v)
			continue
		}
		if op.Method == "GET" { // rule 3
			add("report")
			continue
		}
		add("action:" + actionSeg(op.Path)) // rule 4
	}

	// Collapse a full CRUD set to "crud"; a partial set stays verb-by-verb.
	if len(crud) == len(crudVerbs) {
		crud = []string{"crud"}
	}
	sort.Strings(rest) // action:* after config/report alphabetically: action < config < report — fix below
	// Order: config, report, then action:* (sort.Strings puts action:* first;
	// re-partition for the documented display order).
	var cfg, rep, acts []string
	for _, v := range rest {
		switch {
		case v == "config":
			cfg = append(cfg, v)
		case v == "report":
			rep = append(rep, v)
		default:
			acts = append(acts, v)
		}
	}
	out := append(crud, cfg...)
	out = append(out, rep...)
	return append(out, acts...)
}

// actionSeg names a rule-4 action from the last non-{param} path segment, so
// `/settings/siem/test` → "test" and a path ending in a placeholder names its
// last static segment instead of the brace token.
func actionSeg(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if !strings.HasPrefix(segs[i], "{") {
			return segs[i]
		}
	}
	return strings.ToLower(path) // unreachable for catalog paths; defensive
}
