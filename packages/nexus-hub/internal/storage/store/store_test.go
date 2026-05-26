package store_test

import (
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// TestNewWithPgxPool verifies that the test-only constructor returns a non-nil
// Store and that Pool() returns nil (no concrete *pgxpool.Pool was injected).
func TestNewWithPgxPool(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	s := store.NewWithPgxPool(mock)
	if s == nil {
		t.Fatal("NewWithPgxPool must return non-nil Store")
	}
	if s.Pool() != nil {
		t.Error("Pool() must return nil for test-only constructor (no *pgxpool.Pool)")
	}
}

// TestStoreAccessors verifies that each sub-store accessor returns a non-nil
// value, confirming the sub-store constructors were called correctly.
func TestStoreAccessors(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	s := store.NewWithPgxPool(mock)

	if s.AuthStore() == nil {
		t.Error("AuthStore() must be non-nil")
	}
	if s.EnrollStore() == nil {
		t.Error("EnrollStore() must be non-nil")
	}
	if s.UserStore() == nil {
		t.Error("UserStore() must be non-nil")
	}
	if s.RegistryStore() == nil {
		t.Error("RegistryStore() must be non-nil")
	}
	if s.ConfigStore() == nil {
		t.Error("ConfigStore() must be non-nil")
	}
	if s.OverrideStore() == nil {
		t.Error("OverrideStore() must be non-nil")
	}
	if s.SmartGroupStore() == nil {
		t.Error("SmartGroupStore() must be non-nil")
	}
	if s.TrafficStore() == nil {
		t.Error("TrafficStore() must be non-nil")
	}
}

// TestNewOverrideState_Empty verifies that NewOverrideState with empty bytes
// returns ErrEmptyState (the spec contract requires non-empty state).
func TestNewOverrideState_Empty(t *testing.T) {
	_, err := store.NewOverrideState(nil)
	if err == nil {
		t.Error("NewOverrideState(nil) must return error (override state cannot be empty)")
	}
}

// TestNewOverrideState_ValidJSON verifies that well-formed JSON is parsed
// into a valid OverrideState.
func TestNewOverrideState_ValidJSON(t *testing.T) {
	raw := []byte(`{"global":{"model":"gpt-4"},"perModel":{}}`)
	os, err := store.NewOverrideState(raw)
	if err != nil {
		t.Fatalf("NewOverrideState(valid): %v", err)
	}
	_ = os
}

// TestNewOverrideState_InvalidJSON verifies that malformed JSON is rejected.
func TestNewOverrideState_InvalidJSON(t *testing.T) {
	_, err := store.NewOverrideState([]byte(`{invalid`))
	if err == nil {
		t.Error("NewOverrideState(invalid JSON) must return error")
	}
}

// TestNewOverrideState_NotObject verifies that a non-object top-level JSON
// value (array, scalar) is rejected.
func TestNewOverrideState_NotObject(t *testing.T) {
	_, err := store.NewOverrideState([]byte(`[1,2,3]`))
	if err == nil {
		t.Error("NewOverrideState([...]) must return error for non-object")
	}
}
