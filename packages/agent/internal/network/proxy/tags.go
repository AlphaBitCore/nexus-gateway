package proxy

import "sort"

// mergeTagSets returns the sorted, deduplicated union of a and b. The agent
// accumulates compliance tags across request- and response-stage hook runs,
// so the merger must be stable, deterministic (sorted output), and
// de-duplicating. Callers pass the current ComplianceTags as a and the
// freshly emitted hookResult.Tags as b; the result replaces the field.
func mergeTagSets(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, t := range a {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range b {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
