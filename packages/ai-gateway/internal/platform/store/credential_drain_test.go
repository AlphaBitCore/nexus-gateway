package store

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// `GetCredentialForProvider` and `ListEnabledForProvider` answer the same
// question — "which credentials may serve this provider?" — and must not
// answer it differently. A list excluding `selectionWeight = 0` where the single-row
// query does not leaves the drained credential serving traffic: the resolver falls back to the single row
// whenever the list comes back empty, which is exactly what draining every
// credential to weight 0 produces, while the console shows
// weight 0.
//
// pgxmock replays columns by position and never executes SQL, so asserting on
// the returned struct cannot see a missing WHERE clause at all. The predicate
// has to be pinned into the ExpectQuery regex, which is what these arms do.

func TestGetCredentialForProvider_ExcludesDrainedCredentials(t *testing.T) {
	mock, db := newMockDB(t)
	// The regex IS the assertion: if the predicate changes, no expectation
	// matches and the call fails. The WHOLE clause is anchored, not three
	// islands joined by `[\s\S]*` — that weaker form let an AND->OR slip
	// through, and `(providerId AND enabled AND active) OR weight > 0` returns
	// every provider's credentials, a strictly worse defect than the one being
	// fixed here.
	mock.ExpectQuery(usableCredentialWhere).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(credentialTestColumns).AddRow(makeCredentialRow("c1")...))

	got, err := db.GetCredentialForProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetCredentialForProvider: %v", err)
	}
	if got == nil || got.ID != "c1" {
		t.Fatalf("got %+v, want c1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// usableCredentialWhere anchors the ENTIRE usability predicate, operators
// included. Both queries must match it exactly.
const usableCredentialWhere = `WHERE "providerId" = \$1\s+AND enabled = true\s+` +
	`AND COALESCE\(status, 'active'\) = 'active'\s+` +
	`AND COALESCE\("selectionWeight", 100\) > 0`

// The two queries must keep agreeing. Asserting them in one test is the point:
// they diverged once, silently, and the divergence WAS the defect.
func TestCredentialUsabilityPredicate_IsTheSameInBothQueries(t *testing.T) {
	const usable = usableCredentialWhere

	t.Run("single", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(usable).WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(credentialTestColumns).AddRow(makeCredentialRow("c1")...))
		if _, err := db.GetCredentialForProvider(context.Background(), "p1"); err != nil {
			t.Fatalf("GetCredentialForProvider: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(usable).WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(credentialTestColumns).AddRow(makeCredentialRow("c1")...))
		if _, err := db.ListEnabledForProvider(context.Background(), "p1"); err != nil {
			t.Fatalf("ListEnabledForProvider: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}
