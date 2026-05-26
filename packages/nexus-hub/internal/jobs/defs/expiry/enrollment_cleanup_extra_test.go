package expiry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeEnrollStore stubs the enrollmentTokenStore interface used by
// EnrollmentTokenCleanup without touching a live database.
type fakeEnrollStore struct {
	n   int64
	err error
}

func (f *fakeEnrollStore) CleanupExpiredEnrollmentTokens(_ context.Context) (int64, error) {
	return f.n, f.err
}

// TestEnrollmentTokenCleanup_RunHappyPath verifies that a successful cleanup
// returns nil and uses the correct job metadata.
func TestEnrollmentTokenCleanup_RunHappyPath(t *testing.T) {
	j := &EnrollmentTokenCleanup{
		store:    &fakeEnrollStore{n: 3},
		interval: 5 * time.Minute,
		logger:   testLogger().With("job", "enrollment-cleanup"),
	}

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if j.ID() != enrollmentCleanupJobID {
		t.Errorf("ID = %q; want %q", j.ID(), enrollmentCleanupJobID)
	}
	if j.Name() != enrollmentCleanupJobName {
		t.Errorf("Name = %q; want %q", j.Name(), enrollmentCleanupJobName)
	}
	if j.Description() != enrollmentCleanupJobDescription {
		t.Errorf("Description = %q", j.Description())
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v; want 5m", j.Interval())
	}
}

// TestEnrollmentTokenCleanup_RunZeroDeleted verifies that n=0 is a success
// (no-op) and does not trigger the info log branch (which is not an error).
func TestEnrollmentTokenCleanup_RunZeroDeleted(t *testing.T) {
	j := &EnrollmentTokenCleanup{
		store:    &fakeEnrollStore{n: 0},
		interval: time.Minute,
		logger:   testLogger().With("job", "enrollment-cleanup"),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run (zero deleted): %v", err)
	}
}

// TestEnrollmentTokenCleanup_RunDBError verifies that a DB error propagates.
func TestEnrollmentTokenCleanup_RunDBError(t *testing.T) {
	want := errors.New("db error")
	j := &EnrollmentTokenCleanup{
		store:    &fakeEnrollStore{err: want},
		interval: time.Minute,
		logger:   testLogger().With("job", "enrollment-cleanup"),
	}
	err := j.Run(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("Run expected DB error; got %v", err)
	}
}
