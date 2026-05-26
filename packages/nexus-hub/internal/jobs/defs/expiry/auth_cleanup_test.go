package expiry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeAuthStore records calls and returns scripted results.
type fakeAuthStore struct {
	revokedN   int64
	revokedErr error
	refreshN   int64
	refreshErr error

	revokedCalls int
	refreshCalls int
}

func (f *fakeAuthStore) DeleteExpiredRevokedTokens(_ context.Context) (int64, error) {
	f.revokedCalls++
	return f.revokedN, f.revokedErr
}

func (f *fakeAuthStore) DeleteExpiredRefreshTokens(_ context.Context) (int64, error) {
	f.refreshCalls++
	return f.refreshN, f.refreshErr
}

func TestAuthCleanupJob_IdentityAndInterval(t *testing.T) {
	j := NewAuthCleanup(&fakeAuthStore{}, time.Hour, testLogger())
	if j.ID() != "auth-cleanup" {
		t.Errorf("ID = %q, want auth-cleanup", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h", j.Interval())
	}
}

func TestAuthCleanupJob_ZeroIntervalDefaultsToHour(t *testing.T) {
	j := NewAuthCleanup(&fakeAuthStore{}, 0, testLogger())
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h for zero input", j.Interval())
	}
}

func TestAuthCleanupJob_NegativeIntervalDefaultsToHour(t *testing.T) {
	j := NewAuthCleanup(&fakeAuthStore{}, -time.Second, testLogger())
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h for negative input", j.Interval())
	}
}

func TestAuthCleanupJob_Run_BothDeletesCalled(t *testing.T) {
	fake := &fakeAuthStore{revokedN: 3, refreshN: 5}
	j := NewAuthCleanup(fake, time.Hour, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.revokedCalls != 1 {
		t.Errorf("revokedCalls = %d, want 1", fake.revokedCalls)
	}
	if fake.refreshCalls != 1 {
		t.Errorf("refreshCalls = %d, want 1", fake.refreshCalls)
	}
}

func TestAuthCleanupJob_Run_BothRunEvenWhenRevokedFails(t *testing.T) {
	// If RevokedToken delete fails, RefreshToken delete must still run.
	sentinel := errors.New("db error")
	fake := &fakeAuthStore{revokedErr: sentinel, refreshN: 2}
	j := NewAuthCleanup(fake, time.Hour, testLogger())
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
	if fake.revokedCalls != 1 {
		t.Errorf("revokedCalls = %d, want 1", fake.revokedCalls)
	}
	if fake.refreshCalls != 1 {
		t.Errorf("refreshCalls = %d, want 1 (must run despite revoked failure)", fake.refreshCalls)
	}
}

func TestAuthCleanupJob_Run_BothErrorsJoined(t *testing.T) {
	errA := errors.New("revoked db error")
	errB := errors.New("refresh db error")
	fake := &fakeAuthStore{revokedErr: errA, refreshErr: errB}
	j := NewAuthCleanup(fake, time.Hour, testLogger())
	err := j.Run(context.Background())
	if !errors.Is(err, errA) {
		t.Errorf("err does not contain errA: %v", err)
	}
	if !errors.Is(err, errB) {
		t.Errorf("err does not contain errB: %v", err)
	}
}

func TestAuthCleanupJob_Run_ZeroCountsNoError(t *testing.T) {
	fake := &fakeAuthStore{revokedN: 0, refreshN: 0}
	j := NewAuthCleanup(fake, time.Hour, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
