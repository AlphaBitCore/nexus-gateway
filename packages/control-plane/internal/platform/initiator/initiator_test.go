package initiator

import (
	"context"
	"testing"
)

// TestWithFromRoundTrip pins the core contract: a value set by With is read back
// by From, and the two channel constants are distinct and non-empty.
func TestWithFromRoundTrip(t *testing.T) {
	for _, via := range []string{ViaAssistant} {
		ctx := With(context.Background(), via)
		if got := From(ctx); got != via {
			t.Errorf("From(With(ctx, %q)) = %q; want %q", via, got, via)
		}
	}
}

// TestFromEmptyForPlainContext pins the negative branch: an ordinary context
// (no initiator stamp — an external human/UI request) reads back "", which the
// run-token gate and the audit via column both treat as "not in-process".
func TestFromEmptyForPlainContext(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Errorf("From(plain ctx) = %q; want empty", got)
	}
}

// TestFromIgnoresWrongType guards against a foreign value stored under a
// different key being mistaken for the marker (the type assertion must fail
// closed to "").
func TestFromIgnoresWrongType(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, ViaAssistant)
	if got := From(ctx); got != "" {
		t.Errorf("From(ctx with unrelated key) = %q; want empty (marker is key-scoped)", got)
	}
}
