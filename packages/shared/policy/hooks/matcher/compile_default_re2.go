//go:build !vectorscan

package matcher

// CompileDefault selects the build-tag-appropriate compiler: the pure-Go RE2
// matcher for the default build. A build tagged `vectorscan` swaps in the cgo
// compiler via compile_default_vectorscan.go. See that file for the rationale.
func CompileDefault(pats []Pattern) (Matcher, []BadPattern) { return CompileRE2(pats) }
