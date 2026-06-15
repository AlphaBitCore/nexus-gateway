package specutil

import "fmt"

// ValidateEmbeddingRowCount asserts that a batch-embedding provider
// returned exactly one vector per request input. expected is the number
// of inputs the request carried; got is the number of embedding rows the
// codec decoded from the response.
//
// Every batch embedding decoder re-indexes the response vectors 0..n-1 by
// upstream position and the gateway hands them back to the caller on those
// indices. If a provider silently drops or reorders an item, the vectors
// align to the wrong inputs while still presenting plausible indices —
// silent corruption the caller cannot detect. Converting that into a named
// decode failure (the dispatcher wraps a non-nil decode error into a 502)
// is strictly safer than serving misaligned vectors.
//
// expected <= 0 disables the check: the request input count is unknown
// (e.g. a direct unit-test decode with no DecodeContext, or an empty
// input), so the guard fails open rather than rejecting a valid response.
func ValidateEmbeddingRowCount(expected, got int) error {
	if expected <= 0 || expected == got {
		return nil
	}
	return fmt.Errorf("embedding count mismatch: request had %d input(s) but upstream returned %d embedding(s)", expected, got)
}
