package helpers

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedVerdict is what RequireSeeded decided to do about a row a scenario could
// not find.
type SeedVerdict int

const (
	// SeedSkip — this database was never seeded. An environment precondition.
	SeedSkip SeedVerdict = iota
	// SeedFail — the seed ran and the row is gone. A regression.
	SeedFail
)

// RowCounter is the seam the decision reads the world through, so the decision
// can be tested without a database.
type RowCounter func(ctx context.Context, table string) (int, error)

// SeedDecision separates "this environment was never seeded" from "the seed ran
// but the row this scenario needs is gone".
//
// That distinction is the whole point. Scenarios answered both with t.Skip —
// "no provider seeded locally", "no pii-outbound-scanner hook seeded" — but
// those rows are not environmental facts: Provider.json ships seven providers
// and HookConfig.json ships the pii-outbound-scanner. On a seeded database
// their absence is a regression, and skipping means the scenario goes quiet
// exactly when it should go red. Same false-green shape as an assertion that
// prints FAIL and records PASS.
//
// Deciding is split from acting because t.Skipf and t.Fatalf both call
// runtime.Goexit, so a decision made inside them cannot be tested.
func SeedDecision(ctx context.Context, count RowCounter, baselineTable, missingWhat string) (SeedVerdict, string, error) {
	n, err := count(ctx, baselineTable)
	if err != nil {
		return SeedFail, "", fmt.Errorf("cannot tell whether this database is seeded (%s): %w", baselineTable, err)
	}
	if n == 0 {
		return SeedSkip, fmt.Sprintf(
			"this database has no %s rows at all — it was never seeded. Run "+
				"`cd tools/db-migrate && npm run seed`, then re-run. Skipping rather "+
				"than failing: an unseeded database is an environment precondition, "+
				"not a regression.", baselineTable), nil
	}
	return SeedFail, fmt.Sprintf(
		"the database IS seeded (%d %s rows) but %s is missing — something removed "+
			"it. This is a regression, not a missing precondition, which is why it "+
			"fails instead of skipping.", n, baselineTable, missingWhat), nil
}

// RequireSeeded applies SeedDecision. Call it where a scenario used to skip on
// a row the seed guarantees.
func RequireSeeded(t *testing.T, ctx context.Context, count RowCounter, baselineTable, missingWhat string) {
	t.Helper()
	verdict, msg, err := SeedDecision(ctx, count, baselineTable, missingWhat)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if verdict == SeedSkip {
		t.Skip(msg)
	}
	t.Fatal(msg)
}

// PoolRowCounter counts rows in a seed-owned table. Table names come from this
// package's callers as constants, never from scenario input.
func PoolRowCounter(pool *pgxpool.Pool) RowCounter {
	return func(ctx context.Context, table string) (int, error) {
		var n int
		err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q`, table)).Scan(&n)
		return n, err
	}
}
