package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// A verdict computed from a window that has since emptied is not a verdict.
//
// The rollup SELECTs traffic_event over the window and GROUPs BY provider, so
// it only ever upserts providers that appear IN that window. A provider that
// stops receiving traffic keeps whatever status it last got, indefinitely, and
// an operator cannot clear it because the status is derived.
//
// Seen in production: google-gemini sat at status=unavailable with
// sampleCount=1 — one failed request, hours earlier — while later runs updated
// the providers that did have traffic and left that row untouched.
//
// The reset must run even when the window is completely empty, which is the
// case the early return used to skip.
func TestProviderHealthRollup_ClearsVerdictsWhoseWindowHasPassed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trafficRow bool
	}{
		{"with traffic in the window", true},
		{"with an empty window — the case the early return skipped", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock: %v", err)
			}
			defer mock.Close()
			// pgxmock matches in declaration order by default; this test cares
			// that BOTH calls happen, not which lands first.
			mock.MatchExpectationsInOrder(false)

			rows := pgxmock.NewRows([]string{"pid", "pname", "total", "errors", "avg_latency_ms", "last_request_at", "last_error_at"})
			if tc.trafficRow {
				now := time.Now().UTC()
				lastErr := now.Add(-2 * time.Minute)
				rows = rows.AddRow("prov-1", "p1", int(100), int(1), int(120), now, &lastErr)
			}
			mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).WillReturnRows(rows)
			if tc.trafficRow {
				// Nine AnyArg: ExpectExec with no WithArgs means "expects ZERO
				// arguments" in pgxmock, so an under-specified expectation never
				// matches and — without ExpectationsWereMet — never says so.
				mock.ExpectExec(`INSERT INTO "ProviderHealth"`).
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
						pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
						pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnResult(pgxmock.NewResult("INSERT", 1))
			}
			// The reset. pgxmock matches the SQL by regex and never executes it,
			// so the predicate itself is pinned here — a reset that forgot its
			// WHERE would wipe the verdicts it was meant to preserve.
			mock.ExpectExec(`UPDATE "ProviderHealth"[\s\S]*"windowStart" <`).
				WithArgs(pgxmock.AnyArg()).
				WillReturnResult(pgxmock.NewResult("UPDATE", 1))

			j := &ProviderHealthRollupJob{pool: mock, logger: testLogger()}
			if err := j.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("the stale-verdict reset did not run: %v", err)
			}
		})
	}
}
