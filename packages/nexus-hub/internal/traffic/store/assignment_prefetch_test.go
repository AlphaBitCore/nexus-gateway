package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// pgxmock replays the columns it is given BY POSITION and never executes the
// SQL, so this list — not the query text — is what the scan is asserted
// against. A column added to the statement without being added here scans the
// wrong value into the wrong field, and a test that only checked err == nil
// would stay green through it.
func windowRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"ip_address", "user_id", "device_id", "displayName", "email",
		"assigned_at", "released_at",
	})
}

func TestFindAssignmentsByIPsOverlapping_ScansEveryColumn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	assigned := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	released := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(windowRows().
			AddRow("10.0.0.5", "user-1", "dev-1", "Alice", "a@example.com", assigned, &released).
			AddRow("10.0.0.6", "user-2", "dev-2", "Bob", "", assigned, nil))

	s := New(mock)
	got, err := s.FindAssignmentsByIPsOverlapping(context.Background(),
		[]string{"10.0.0.5", "10.0.0.6"}, assigned, released)
	if err != nil {
		t.Fatalf("FindAssignmentsByIPsOverlapping: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2", len(got))
	}

	first := got[0]
	if first.IP != "10.0.0.5" || first.UserID != "user-1" || first.DeviceID != "dev-1" ||
		first.DisplayName != "Alice" || first.Email != "a@example.com" {
		t.Errorf("first window scanned wrong: %+v", first)
	}
	if !first.AssignedAt.Equal(assigned) {
		t.Errorf("assignedAt = %v, want %v", first.AssignedAt, assigned)
	}
	if first.ReleasedAt == nil || !first.ReleasedAt.Equal(released) {
		t.Errorf("releasedAt = %v, want %v", first.ReleasedAt, released)
	}
	if got[1].ReleasedAt != nil {
		t.Errorf("an open-ended assignment must scan a NULL released_at as nil, got %v",
			got[1].ReleasedAt)
	}
}

func TestFindAssignmentsByIPsOverlapping_NoIPsIsNotAQuery(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	s := New(mock)
	got, err := s.FindAssignmentsByIPsOverlapping(context.Background(), nil, time.Now(), time.Now())
	if err != nil || got != nil {
		t.Fatalf("empty IP set: got %v / %v, want nil / nil", got, err)
	}
	// No expectation was declared, so a query here would be an unexpected call.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestFindAssignmentsByIPsOverlapping_RefusesRatherThanTruncates is the property
// that matters more than the speed. A truncated read can turn an IP that several
// assignments share into one that appears unique, and the caller then names a
// user confidently and wrongly. Leaving the rows for the next run is
// recoverable; a wrong attribution written into the audit trail is not.
func TestFindAssignmentsByIPsOverlapping_RefusesRatherThanTruncates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	assigned := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	rows := windowRows()
	for i := range maxAssignmentPrefetchRows + 1 {
		rows.AddRow("10.0.0."+strconv.Itoa(i%250), "u"+strconv.Itoa(i), "d", "N", "", assigned, nil)
	}
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	s := New(mock)
	_, err := s.FindAssignmentsByIPsOverlapping(context.Background(),
		[]string{"10.0.0.5"}, assigned, assigned)
	if err == nil {
		t.Fatal("a read past the cap returned rows instead of refusing; a truncated assignment " +
			"set makes a shared IP look unique, which is a confidently wrong attribution")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("err = %v, want it to say it refused rather than truncated", err)
	}
}

func TestFindAssignmentsByIPsOverlapping_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("connection reset"))

	s := New(mock)
	if _, err := s.FindAssignmentsByIPsOverlapping(context.Background(),
		[]string{"10.0.0.5"}, time.Now(), time.Now()); err == nil {
		t.Fatal("a failed query reported success; the caller would read the empty result as " +
			"'no assignment matches' and stamp a terminal verdict on rows nothing looked at")
	}
}
