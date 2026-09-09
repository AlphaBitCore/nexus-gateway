package dsarstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// The ownership rule, on its own. Every arm is a way an erasure goes wrong in a
// direction someone notices: too narrow leaves the subject's dead-lettered
// events on disk after they were told the data was gone; too wide deletes the
// record of whoever held the device before or after them.
func TestDLQRowBelongsTo(t *testing.T) {
	released := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	windows := []deviceWindow{
		{deviceID: "dev-open", assignedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{deviceID: "dev-closed", assignedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), releasedAt: &released},
	}
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("fixture time %q: %v", s, err)
		}
		return ts
	}

	for _, tc := range []struct {
		name string
		env  dlqEnvelope
		want bool
	}{
		{"gateway event tagged with the subject", dlqEnvelope{Source: "ai-gateway", EntityID: sp("subj1")}, true},
		{"gateway event tagged with someone else", dlqEnvelope{Source: "ai-gateway", EntityID: sp("subj2")}, false},
		{"gateway event with no entity at all", dlqEnvelope{Source: "ai-gateway"}, false},

		{"agent event on a device the subject still holds",
			dlqEnvelope{Source: "agent", ThingID: sp("dev-open"), Timestamp: at("2026-06-01T00:00:00Z")}, true},
		{"agent event one second before the assignment — the previous holder's",
			dlqEnvelope{Source: "agent", ThingID: sp("dev-open"), Timestamp: at("2026-02-28T23:59:59Z")}, false},
		{"agent event exactly at the assignment instant",
			dlqEnvelope{Source: "agent", ThingID: sp("dev-open"), Timestamp: at("2026-03-01T00:00:00Z")}, true},
		{"agent event inside a closed window",
			dlqEnvelope{Source: "agent", ThingID: sp("dev-closed"), Timestamp: at("2026-03-15T00:00:00Z")}, true},
		{"agent event at the release instant — the next holder's",
			dlqEnvelope{Source: "agent", ThingID: sp("dev-closed"), Timestamp: at("2026-04-01T00:00:00Z")}, false},
		{"agent event on a device the subject never held",
			dlqEnvelope{Source: "agent", ThingID: sp("dev-other"), Timestamp: at("2026-03-15T00:00:00Z")}, false},
		{"agent event with no device", dlqEnvelope{Source: "agent", Timestamp: at("2026-03-15T00:00:00Z")}, false},

		{"a source this store does not know", dlqEnvelope{Source: "compliance-proxy", EntityID: sp("subj1")}, false},
		{"an empty envelope", dlqEnvelope{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dlqRowBelongsTo(tc.env, "subj1", windows); got != tc.want {
				t.Errorf("dlqRowBelongsTo = %v; want %v", got, tc.want)
			}
		})
	}
}

// A DLQ stage that cannot read the queue must fail the erasure, not report a
// clean sweep. Every arm below is a way the read can break; each one returning
// nil would tell the operator the subject's dead-lettered events were removed
// when they are still on disk.
//
// The two rows.Err() guards are not among them. pgxmock delivers a RowError on
// the Nth Scan rather than at the end of iteration, so arms written for those
// guards re-entered the scan branch instead — they were duplicates wearing a
// different name, which is worse than an uncovered line because the suite then
// claims a path it never took. Both guards stay in the code: a cursor that
// breaks after the last row is a real failure against a real driver, and it is
// the difference between an erasure that stops and one that reports a clean
// sweep of a queue it stopped reading.
func TestEraseSubjectDLQ_FailuresAreNotSilentSuccess(t *testing.T) {
	winCols := []string{"deviceId", "assignedAt", "releasedAt"}
	dlqCols := []string{"id", "payload"}

	for _, tc := range []struct {
		name  string
		setup func(m pgxmock.PgxPoolIface)
		want  string
	}{
		{
			name: "the assignment windows cannot be read",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("subj1").
					WillReturnError(errors.New("boom"))
			},
			want: "read device assignments",
		},
		{
			name: "an assignment row does not scan",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("subj1").
					WillReturnRows(pgxmock.NewRows(winCols).AddRow("dev-1", "not a time", (*time.Time)(nil)))
			},
			want: "scan device assignment",
		},
		{
			name: "the queue itself cannot be read",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("subj1").
					WillReturnRows(pgxmock.NewRows(winCols))
				m.ExpectQuery(`SELECT id, payload FROM traffic_event_dlq`).
					WillReturnError(errors.New("boom"))
			},
			want: "scan dlq for erasure",
		},
		{
			name: "a queue row does not scan",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("subj1").
					WillReturnRows(pgxmock.NewRows(winCols))
				m.ExpectQuery(`SELECT id, payload FROM traffic_event_dlq`).
					WillReturnRows(pgxmock.NewRows(dlqCols).AddRow("dlq-1", struct{ X int }{1}))
			},
			want: "scan dlq row",
		},
		{
			name: "the delete is refused",
			setup: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("subj1").
					WillReturnRows(pgxmock.NewRows(winCols))
				m.ExpectQuery(`SELECT id, payload FROM traffic_event_dlq`).
					WillReturnRows(pgxmock.NewRows(dlqCols).
						AddRow("dlq-1", []byte(`{"source":"ai-gateway","entityId":"subj1"}`)))
				m.ExpectExec(`DELETE FROM traffic_event_dlq`).WithArgs([]string{"dlq-1"}).
					WillReturnError(errors.New("boom"))
			},
			want: "delete subject dlq rows",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("mock pool: %v", err)
			}
			defer m.Close()
			m.ExpectBegin()
			tc.setup(m)
			tx, err := m.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			n, err := eraseSubjectDLQ(context.Background(), tx, "subj1")
			if err == nil {
				t.Fatalf("erasure reported success (%d rows) on a queue it could not read", n)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want it to name %q so an operator can tell which read failed",
					err, tc.want)
			}
			if n != 0 {
				t.Errorf("count = %d on a failed stage; a non-zero count would be reported as work done", n)
			}
		})
	}
}

// An empty queue is the common case and must cost nothing: no DELETE is issued
// at all. pgxmock fails the expectation check if one is, which is the assertion.
func TestEraseSubjectDLQ_NoMatchIssuesNoDelete(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock pool: %v", err)
	}
	defer m.Close()
	m.ExpectBegin()
	m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId", "assignedAt", "releasedAt"}))
	m.ExpectQuery(`SELECT id, payload FROM traffic_event_dlq`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "payload"}).
			AddRow("dlq-other", []byte(`{"source":"ai-gateway","entityId":"subj2"}`)))

	tx, err := m.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	n, err := eraseSubjectDLQ(context.Background(), tx, "subj1")
	if err != nil {
		t.Fatalf("eraseSubjectDLQ: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d; want 0 — nothing in the queue is this subject's", n)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected statement issued: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
