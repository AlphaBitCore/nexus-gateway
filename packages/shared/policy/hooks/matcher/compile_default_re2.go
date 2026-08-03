//go:build !vectorscan

package matcher

// CompileDefault selects the build-tag-appropriate compiler: the pure-Go RE2
// matcher for the default build. A build tagged `vectorscan` swaps in the cgo
// compiler via compile_default_vectorscan.go. See that file for the rationale.
func CompileDefault(pats []Pattern) (Matcher, []BadPattern) { return CompileRE2(pats) }

// DescribeEngine reports the RE2 matcher this untagged build compiled in.
func DescribeEngine() Engine {
	return Engine{
		Name:       "re2",
		SinglePass: false,
		Effect: "pure-Go regexp: every pattern is scanned separately over every segment, so cost grows with " +
			"body size AND pattern count — 423 rules over a 400 KB body measured 4.3 s sequentially. The scans " +
			"are fanned out across cores above a size threshold, which cuts the latency but not the total work. " +
			"Build with -tags vectorscan for the single-pass engine.",
	}
}
