package expiry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeEnrollmentStore is a hand-rolled fake satisfying enrollmentTokenStore.
type fakeEnrollmentStore struct {
	calls int
	ret   int64
	err   error
}

func (f *fakeEnrollmentStore) CleanupExpiredEnrollmentTokens(context.Context) (int64, error) {
	f.calls++
	return f.ret, f.err
}

func TestEnrollmentTokenCleanup_Identity(t *testing.T) {
	j := &EnrollmentTokenCleanup{
		store:    &fakeEnrollmentStore{},
		interval: 15 * time.Minute,
		logger:   testLogger(),
	}
	if j.ID() != enrollmentCleanupJobID {
		t.Errorf("ID = %q, want %q", j.ID(), enrollmentCleanupJobID)
	}
	if j.Name() != enrollmentCleanupJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() != enrollmentCleanupJobDescription {
		t.Errorf("Description = %q", j.Description())
	}
	if j.Interval() != 15*time.Minute {
		t.Errorf("Interval = %v", j.Interval())
	}
}

func TestEnrollmentTokenCleanup_Run_Success(t *testing.T) {
	fs := &fakeEnrollmentStore{ret: 3}
	j := &EnrollmentTokenCleanup{store: fs, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fs.calls != 1 {
		t.Errorf("calls = %d, want 1", fs.calls)
	}
}

func TestEnrollmentTokenCleanup_Run_ZeroIsNoLog(t *testing.T) {
	fs := &fakeEnrollmentStore{ret: 0}
	j := &EnrollmentTokenCleanup{store: fs, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestEnrollmentTokenCleanup_Run_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	fs := &fakeEnrollmentStore{err: sentinel}
	j := &EnrollmentTokenCleanup{store: fs, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
