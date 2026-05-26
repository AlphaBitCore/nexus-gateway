package http

import "context"

type reqIDKey struct{}

// WithRequestID returns a context carrying the given request id.
// Storing an empty string is permitted; readers see "".
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// RequestIDFromContext returns the id stored by WithRequestID, or "" if
// none is set or the value is not a string.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(reqIDKey{}).(string)
	return v
}
