package helpers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func counter(n int, err error) RowCounter {
	return func(context.Context, string) (int, error) { return n, err }
}

// An unseeded database is an environment precondition, so it skips — and the
// message must name the command, or the reader is left guessing.
func TestSeedDecision_UnseededDatabaseSkips(t *testing.T) {
	v, msg, err := SeedDecision(context.Background(), counter(0, nil), "Provider", "the x hook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != SeedSkip {
		t.Fatal("an unseeded database must skip, not fail — it is a precondition, not a regression")
	}
	if !strings.Contains(msg, "npm run seed") {
		t.Errorf("the skip must name the fix; got %q", msg)
	}
}

// The case this helper exists for. Scenarios used to skip here, which is how a
// seed regression stays invisible: the coverage disappears instead of going red.
func TestSeedDecision_SeededButRowMissingFails(t *testing.T) {
	v, msg, err := SeedDecision(context.Background(), counter(7, nil), "Provider", "the pii-outbound-scanner hook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != SeedFail {
		t.Fatal("a missing row on a SEEDED database must fail; skipping is how the regression stays invisible")
	}
	for _, want := range []string{"regression", "pii-outbound-scanner", "7 Provider"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure must contain %q so the reader knows it is not a precondition; got %q", want, msg)
		}
	}
}

// A database we cannot query has told us nothing, so it must not be read as
// either answer — least of all as "never seeded", which would turn an outage
// into silent coverage loss.
func TestSeedDecision_UnreadableDatabaseIsAnError(t *testing.T) {
	v, _, err := SeedDecision(context.Background(), counter(0, errors.New("boom")), "Provider", "anything")
	if err == nil {
		t.Fatal("a failed count must surface as an error, not a verdict")
	}
	if v == SeedSkip {
		t.Error("a failed count must never be reported as 'never seeded'")
	}
}
