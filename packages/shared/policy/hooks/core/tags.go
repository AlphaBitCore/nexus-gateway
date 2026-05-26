package core

// AppendTag appends t to tags only if not already present. Returns the
// possibly extended slice. O(n); acceptable for small per-result tag sets.
func AppendTag(tags []string, t string) []string {
	for _, existing := range tags {
		if existing == t {
			return tags
		}
	}
	return append(tags, t)
}

// appendTag is the unexported alias kept for same-package callers.
func appendTag(tags []string, t string) []string { return AppendTag(tags, t) }

// Severity tags assign a compliance sensitivity level to a hook's output.
// Canonical format: "severity:<level>", lowercase. Downstream audit layers
// persist the merged tag set on traffic_event.compliance_tags (text[]); the
// data-residency hook uses HighestSeverityTag to branch on the most
// restrictive upstream severity.
const (
	SeverityPublic       = "severity:public"
	SeverityInternal     = "severity:internal"
	SeverityConfidential = "severity:confidential"
	SeverityRestricted   = "severity:restricted"
)

// severityRank assigns a numeric rank to each known severity tag for
// comparison. Unknown or non-severity tags have rank 0 and are silently
// ignored by HighestSeverityTag callers.
var severityRank = map[string]int{
	SeverityPublic:       1,
	SeverityInternal:     2,
	SeverityConfidential: 3,
	SeverityRestricted:   4,
}

// HighestSeverityTag returns the most sensitive severity:* tag present in
// tags, or "" when none is set. Used by hooks (e.g. data-residency) that
// branch on the highest upstream severity level rather than iterating all
// tags.
func HighestSeverityTag(tags []string) string {
	best := ""
	bestRank := 0
	for _, t := range tags {
		if r, ok := severityRank[t]; ok && r > bestRank {
			best = t
			bestRank = r
		}
	}
	return best
}
