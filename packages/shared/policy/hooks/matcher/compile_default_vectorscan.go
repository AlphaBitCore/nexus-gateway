//go:build vectorscan

package matcher

// CompileDefault selects the build-tag-appropriate compiler: the Vectorscan
// (cgo) hybrid when built with `-tags vectorscan`. It lets callers outside the
// validators package (e.g. the pipeline's union prefilter) compile a pattern set
// through the same engine the hooks use, without each one re-implementing the
// build-tag selection.
func CompileDefault(pats []Pattern) (Matcher, []BadPattern) { return CompileVectorscan(pats) }
