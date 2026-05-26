package http

import (
	"context"
	"testing"
)

func TestWithRequestID_RoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-abc")
	if got := RequestIDFromContext(ctx); got != "req-abc" {
		t.Errorf("RequestIDFromContext: got %q, want %q", got, "req-abc")
	}
}

func TestRequestIDFromContext_Empty(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext on bare ctx: got %q, want \"\"", got)
	}
}

func TestWithRequestID_EmptyStringStored(t *testing.T) {
	ctx := WithRequestID(context.Background(), "")
	if got := RequestIDFromContext(ctx); got != "" {
		t.Errorf("RequestIDFromContext after WithRequestID(\"\"): got %q, want \"\"", got)
	}
}

func TestWithRequestID_Overwrite(t *testing.T) {
	ctx := WithRequestID(context.Background(), "first")
	ctx = WithRequestID(ctx, "second")
	if got := RequestIDFromContext(ctx); got != "second" {
		t.Errorf("RequestIDFromContext after overwrite: got %q, want %q", got, "second")
	}
}
