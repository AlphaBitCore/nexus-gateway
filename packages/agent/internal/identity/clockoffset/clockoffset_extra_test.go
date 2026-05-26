package clockoffset

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// errGetStore wraps memStore and injects a configurable error from Get(),
// allowing tests to exercise the Load() slog.Warn + return-0 branch that
// fires on any non-ErrNotFound store failure.

type errGetStore struct {
	mu     sync.Mutex
	inner  *memStore
	getErr error
}

func newErrGetStore(getErr error) *errGetStore {
	return &errGetStore{inner: newMemStore(), getErr: getErr}
}

func (e *errGetStore) Set(key string, value []byte) error {
	return e.inner.Set(key, value)
}

func (e *errGetStore) Get(_ string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.getErr != nil {
		return nil, e.getErr
	}
	return nil, secretstore.ErrNotFound
}

func (e *errGetStore) Delete(key string) error { return e.inner.Delete(key) }
func (e *errGetStore) Close() error            { return nil }

// DriftLevel.String — default / unknown arm

// TestDriftLevelString_UnknownValue exercises the default branch of
// DriftLevel.String() which formats unrecognised integer values as
// "unknown(<n>)". This prevents callers from silently receiving an empty
// string when a newer code version introduces a level that an older String()
// implementation has not yet handled.
func TestDriftLevelString_UnknownValue(t *testing.T) {
	unknown := DriftLevel(99)
	got := unknown.String()
	want := fmt.Sprintf("unknown(%d)", 99)
	if got != want {
		t.Errorf("DriftLevel(99).String() = %q; want %q", got, want)
	}
}

// OffsetStore.Load — non-ErrNotFound store error (slog.Warn + return 0)

// TestOffsetStore_Load_StoreError_ReturnsZero verifies that when the
// underlying secretstore.Get returns a non-ErrNotFound error (e.g. permission
// denied, I/O fault), Load() logs a warning via slog and returns 0 rather
// than surfacing the error to callers.  Returning 0 keeps the agent bootable
// when the vault is temporarily unavailable.
func TestOffsetStore_Load_StoreError_ReturnsZero(t *testing.T) {
	storeErr := errors.New("keychain: permission denied")
	os := NewOffsetStore(newErrGetStore(storeErr))
	got := os.Load()
	if got != 0 {
		t.Errorf("Load() on store-error must return 0; got %v", got)
	}
}

// OffsetStore.Save — store write failure surfaces as wrapped error

// TestOffsetStore_Save_WriteError_SurfacesWrappedError verifies that when the
// underlying secretstore.Set returns an error, Save() wraps it with the
// "clockoffset: save offset:" prefix and returns it to the caller.
// Surfacing write failures is the correct behaviour — callers (the enrollment
// flow, token refresh) need to know the offset could not be persisted so they
// can log or retry.
func TestOffsetStore_Save_WriteError_SurfacesWrappedError(t *testing.T) {
	writeErr := errors.New("keychain: write failed")
	s := newMemStore()
	s.setSetErr(writeErr)
	os := NewOffsetStore(s)

	err := os.Save(5 * time.Minute)
	if err == nil {
		t.Fatal("Save() must return an error when the underlying store fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("Save() error chain must contain the original write error; got: %v", err)
	}
}
