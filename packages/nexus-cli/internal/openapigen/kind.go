package openapigen

import (
	"sort"
	"strconv"
	"strings"
)

// assignOperationIDs sets a unique OperationID on every route. The base id comes
// from the handler name (createQuotaPolicy); when one handler is bound to several
// routes of the SAME kind — PUT+PATCH on the same path, or POST on two sibling
// paths — those routes share a base and would violate OpenAPI's operationId-
// uniqueness rule within that kind's document. Uniqueness is enforced PER KIND
// (each kind is a separate OpenAPI document), so the same id may legitimately
// recur across kinds (getConfig on extract-cache and on semantic-cache). A
// collision group is disambiguated deterministically: by the HTTP method when the
// members' methods differ, otherwise by the distinguishing trailing path segment.
func assignOperationIDs(routes []route, basePrefix string) {
	byKind := map[string][]int{}
	for i := range routes {
		k := deriveKind(routes[i].Path, basePrefix)
		byKind[k] = append(byKind[k], i)
	}
	for _, idxs := range byKind {
		assignWithinKind(routes, idxs)
	}
}

// assignWithinKind makes the given routes' operationIds unique among themselves.
func assignWithinKind(routes []route, idxs []int) {
	groups := map[string][]int{}
	for _, i := range idxs {
		b := operationID(routes[i].handlerName, routes[i].Method, routes[i].Path)
		groups[b] = append(groups[b], i)
	}
	used := map[string]bool{}
	for b, g := range groups {
		if len(g) == 1 {
			routes[g[0]].OperationID = b
			used[b] = true
		}
	}
	var bases []string
	for b, g := range groups {
		if len(g) > 1 {
			bases = append(bases, b)
		}
	}
	sort.Strings(bases) // stable, independent of route-discovery order
	for _, b := range bases {
		g := groups[b]
		sort.Slice(g, func(a, c int) bool {
			if routes[g[a]].Path != routes[g[c]].Path {
				return routes[g[a]].Path < routes[g[c]].Path
			}
			return routes[g[a]].Method < routes[g[c]].Method
		})
		sameMethod := true
		for _, i := range g[1:] {
			if routes[i].Method != routes[g[0]].Method {
				sameMethod = false
				break
			}
		}
		for _, i := range g {
			token := lastPathToken(routes[i].Path)
			if !sameMethod {
				token = routes[i].Method
			}
			id := disambiguate(b, token)
			for n := 2; used[id]; n++ { // final guard (rarely reached)
				id = disambiguate(b, token) + strconv.Itoa(n)
			}
			routes[i].OperationID = id
			used[id] = true
		}
	}
}

// disambiguate appends a PascalCase token to base, unless base already ends with
// it (so a "hookTest" handler on a ".../test" path stays "hookTest").
func disambiguate(base, token string) string {
	p := pascalToken(token)
	if p == "" {
		return base
	}
	if strings.HasSuffix(strings.ToLower(base), strings.ToLower(p)) {
		return base
	}
	return base + p
}

// lastPathToken is the final non-parameter segment of a path (the literal that
// distinguishes sibling routes), e.g. /hooks/:id/dry-run -> "dry-run".
func lastPathToken(path string) string {
	segs := strings.Split(path, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i]
		if s == "" || strings.HasPrefix(s, ":") || strings.HasPrefix(s, "{") {
			continue
		}
		return s
	}
	return ""
}

// pascalToken PascalCases a path/method token split on - and _ (dry-run -> DryRun).
func pascalToken(s string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' }) {
		b.WriteString(upperFirst(strings.ToLower(part)))
	}
	return b.String()
}

// deriveKind returns the resource kind for a path: the first path segment after
// the base prefix. /api/admin/quota-policies/{id} -> "quota-policies";
// /api/admin/cache/global -> "cache". Returns "" if the path lies outside the
// base prefix (the caller treats that as unresolved).
func deriveKind(path, basePrefix string) string {
	rest := strings.TrimPrefix(path, strings.TrimRight(basePrefix, "/"))
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	// A path parameter as the first segment has no stable kind name.
	if strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, ":") {
		return ""
	}
	return rest
}

// operationID derives a camelCase operationId. It prefers the Go handler name
// (CreateQuotaPolicy -> createQuotaPolicy); absent that (func literals) it
// synthesises one from the method and path.
func operationID(handlerName, method, path string) string {
	if handlerName != "" {
		return lowerFirst(handlerName)
	}
	cleaned := strings.NewReplacer("/", " ", "{", " ", "}", " ", "-", " ", ":", " ").Replace(path)
	parts := strings.Fields(cleaned)
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, p := range parts {
		b.WriteString(upperFirst(p))
	}
	return b.String()
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
