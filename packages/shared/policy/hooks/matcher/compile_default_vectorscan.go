//go:build vectorscan

package matcher

// CompileDefault selects the build-tag-appropriate compiler: the Vectorscan
// (cgo) hybrid when built with `-tags vectorscan`. It lets callers outside the
// validators package (e.g. the pipeline's union prefilter) compile a pattern set
// through the same engine the hooks use, without each one re-implementing the
// build-tag selection.
func CompileDefault(pats []Pattern) (Matcher, []BadPattern) { return CompileVectorscan(pats) }

// DescribeEngine reports the Vectorscan matcher this tagged build compiled in.
func DescribeEngine() Engine {
	return Engine{
		Name:       "vectorscan",
		SinglePass: true,
		Effect: "single-pass cgo scan: every pattern is evaluated in ONE pass over the text, so cost grows " +
			"with body size but not with pattern count. It reads the whole segment — the detection cap bounds a " +
			"pattern's repeat, not how much text is examined. The intended production engine.",
	}
}
