package spillsweep

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// The property that matters is over-deletion. A reference checker that answers
// "not referenced" about a blob a live row still points at turns the sweep into
// a data-loss engine: the row survives, the body it addresses does not, and the
// Traffic drawer shows a capture whose payload cannot be fetched.
func TestHasSpillRefs_ReferencedKeysSurvive(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock pool: %v", err)
	}
	defer m.Close()

	keys := []string{"live-req", "live-resp", "orphan"}
	m.ExpectQuery(`FROM traffic_event_payload`).WithArgs(keys).
		WillReturnRows(pgxmock.NewRows([]string{"k"}).AddRow("live-req").AddRow("live-resp"))

	got, err := NewDBQuerier(m).HasSpillRefs(context.Background(), keys)
	if err != nil {
		t.Fatalf("HasSpillRefs: %v", err)
	}
	if !got["live-req"] {
		t.Error("a key referenced by a request body was reported unreferenced — the sweep would delete a live payload")
	}
	if !got["live-resp"] {
		t.Error("a key referenced by a RESPONSE body was reported unreferenced; checking only the request column " +
			"leaves every response blob deletable while its row still points at it")
	}
	if got["orphan"] {
		t.Error("an unreferenced key was reported referenced — the orphan is never collected and the bucket grows forever")
	}
}

// Both JSONB columns must appear in the statement. A checker that reads one
// direction passes any test whose fixture only has that direction, and deletes
// every blob of the other.
func TestHasSpillRefs_ChecksBothDirections(t *testing.T) {
	for _, col := range []string{"request_spill_ref", "response_spill_ref"} {
		if !strings.Contains(hasSpillRefsSQL, col) {
			t.Errorf("the reference query does not read %s; blobs in that direction would be swept "+
				"while a live row still references them", col)
		}
	}
	// Extracted by key, not compared as whole JSONB: the stored value is an
	// envelope (backend, key, size, sha256, content_type) and the sweep only
	// ever knows the key.
	if !strings.Contains(hasSpillRefsSQL, "->> 'key'") {
		t.Error("the query does not extract the `key` field; matching the whole JSONB envelope against an " +
			"object key returns nothing, and a checker that matches nothing marks everything deletable")
	}
}

// A failed read must be an error, never an empty map. The sweep reads an empty
// map as "none of these are referenced" and deletes the lot.
func TestHasSpillRefs_FailureIsNeverAnEmptyAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m pgxmock.PgxPoolIface)
		want  string
	}{
		{
			name: "the query is refused",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM traffic_event_payload`).WillReturnError(errors.New("boom"))
			},
			want: "query spill references",
		},
		{
			name: "a row does not scan",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM traffic_event_payload`).WithArgs([]string{"k1"}).
					WillReturnRows(pgxmock.NewRows([]string{"k"}).AddRow(struct{ X int }{1}))
			},
			want: "scan spill reference",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("mock pool: %v", err)
			}
			defer m.Close()
			tc.setup(m)

			got, err := NewDBQuerier(m).HasSpillRefs(context.Background(), []string{"k1"})
			if err == nil {
				t.Fatalf("a failed reference check returned %v and no error — the sweep reads that as "+
					"'nothing is referenced' and deletes every candidate", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want it to name %q", err, tc.want)
			}
			if got != nil {
				t.Errorf("a non-nil map (%v) alongside an error invites a caller to use it", got)
			}
		})
	}
}

// No keys means no statement. A sweep that found nothing age-eligible must not
// pay for a round trip, and `= ANY('{}')` is a query with no purpose.
func TestHasSpillRefs_NoCandidatesIssuesNoQuery(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock pool: %v", err)
	}
	defer m.Close()

	got, err := NewDBQuerier(m).HasSpillRefs(context.Background(), nil)
	if err != nil {
		t.Fatalf("HasSpillRefs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want an empty set", got)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Errorf("a statement was issued for an empty candidate list: %v", err)
	}
}
