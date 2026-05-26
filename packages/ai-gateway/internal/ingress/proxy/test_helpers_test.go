package proxy

// Shared test helpers used by multiple *_test.go files in this
// package. Pull in helpers here when at least two tests want the same
// behavior; don't add helpers that only one test needs (those stay
// local). PR #24 / O7: replaced the bufErrReader (buffer_test) and
// errReadCloser (passthrough_test) near-duplicate yield-then-err
// readers with a single yieldThenErrReader.
//
// Note: errReader in proxy_residuals_test.go is semantically distinct
// (always errors on first Read, no yield) and stays local to that
// file.

// yieldThenErrReader yields `first` on the first Read call and
// returns `err` on every subsequent call. Used to drive io.Reader
// consumers through "one good chunk then a failure" without setting
// up a full upstream / connection. Callers MUST set err to a
// non-nil value — passing nil would return (0, nil) which violates
// the io.Reader contract.
type yieldThenErrReader struct {
	first   []byte
	err     error
	yielded bool
}

func (r *yieldThenErrReader) Read(p []byte) (int, error) {
	if !r.yielded {
		r.yielded = true
		return copy(p, r.first), nil
	}
	return 0, r.err
}
