package http

import "context"

// attemptKey is unexported so callers must use the helpers.
type attemptKey struct{}

// WithAttempt returns a context tagging the outbound HTTP request as
// the n-th attempt. Used by retry middleware to surface attempt counts
// in the outbound debug log line. n must be >= 1; values <= 0 are
// silently coerced to 1.
func WithAttempt(ctx context.Context, n int) context.Context {
	if n < 1 {
		n = 1
	}
	return context.WithValue(ctx, attemptKey{}, n)
}

// AttemptFromContext returns the attempt count stored by WithAttempt,
// or 1 if none is set.
func AttemptFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(attemptKey{}).(int); ok && v >= 1 {
		return v
	}
	return 1
}
