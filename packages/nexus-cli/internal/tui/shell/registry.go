package shell

import "strings"

// esc returns to the cockpit (index 0).
// viewEntry names a dashboard view for the slash palette and the breadcrumb.
// The index of an entry matches the index of its view in Model.views.
type viewEntry struct {
	name    string   // canonical short label shown in the tab row + palette
	aliases []string // extra fuzzy-match tokens (e.g. "traffic" for Radar)
}

// matchesQuery reports whether q fuzzy-matches this entry — a case-insensitive
// substring over the name and aliases. An empty query matches everything.
func (e viewEntry) matchesQuery(q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	if strings.Contains(strings.ToLower(e.name), q) {
		return true
	}
	for _, a := range e.aliases {
		if strings.Contains(strings.ToLower(a), q) {
			return true
		}
	}
	return false
}

// matchEntries returns the indices of entries that fuzzy-match q, preserving
// registry order. It is the lookup behind the command palette.
func matchEntries(entries []viewEntry, q string) []int {
	out := make([]int, 0, len(entries))
	for i, e := range entries {
		if e.matchesQuery(q) {
			out = append(out, i)
		}
	}
	return out
}

// resolveViewIndex returns the index of the view whose name matches name exactly
// (case-insensitive), else the first fuzzy alias match, else -1. It is how the
// Ask-Nexus navigate intent turns a model-supplied view name into a tab index.
func resolveViewIndex(entries []viewEntry, name string) int {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1
	}
	for i, e := range entries {
		if strings.EqualFold(e.name, name) {
			return i
		}
	}
	if idxs := matchEntries(entries, name); len(idxs) > 0 {
		return idxs[0]
	}
	return -1
}
