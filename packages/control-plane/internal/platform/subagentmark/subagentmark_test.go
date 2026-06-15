package subagentmark

import (
	"context"
	"testing"
)

// TestWithFromRoundTrip pins the core contract: a label set by With is read back by
// From, distinct labels are preserved, and the empty context reads back "".
func TestWithFromRoundTrip(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Errorf("From(plain ctx) = %q; want empty", got)
	}
	for _, label := range []string{"subagent 1", "subagent 4"} {
		ctx := With(context.Background(), label)
		if got := From(ctx); got != label {
			t.Errorf("From(With(ctx, %q)) = %q; want %q", label, got, label)
		}
	}
}

// TestFromIgnoresWrongType guards against a foreign value under a different key
// being mistaken for the marker (the assertion must fail closed to "").
func TestFromIgnoresWrongType(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "subagent 9")
	if got := From(ctx); got != "" {
		t.Errorf("From(ctx with unrelated key) = %q; want empty (marker is key-scoped)", got)
	}
}
