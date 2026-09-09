package pipeline

// unbuildableTagPrefix names an implementation the operator configured that
// could not be built. An audit query on it answers "which requests went out
// without the PII detector" rather than "something was wrong once".
const unbuildableTagPrefix = "hook-unbuildable:"

// UnbuildableTags turns a list of implementation ids into the audit tags for
// them, skipping empties.
//
// Exported because the tag has TWO producers and they must not drift. The merge
// stamps it for a pipeline that ran; a caller stamps it directly when
// BuildPipeline returned no pipeline at all, which is what happens when every
// configured hook failed to build. Those are the same fact about the same
// request, so a reader filtering on one prefix has to find both — and two
// string literals in two packages is how that stops being true.
func UnbuildableTags(impls []string) []string {
	if len(impls) == 0 {
		return nil
	}
	out := make([]string, 0, len(impls))
	for _, impl := range impls {
		if impl != "" {
			out = append(out, unbuildableTagPrefix+impl)
		}
	}
	return out
}
