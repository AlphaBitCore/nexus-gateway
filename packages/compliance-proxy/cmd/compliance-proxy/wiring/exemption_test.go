package wiring

import (
	"testing"
)

func TestInitExemptionStore_ReturnsNonNil(t *testing.T) {
	store := InitExemptionStore(testLogger())
	if store == nil {
		t.Fatal("expected non-nil exemption store")
	}
}

func TestInitExemptionStore_SnapshotIsEmpty(t *testing.T) {
	store := InitExemptionStore(testLogger())
	// A freshly-initialised store has no active exemptions.
	snap := store.Snapshot()
	if len(snap.Entries) != 0 {
		t.Errorf("expected empty snapshot on fresh store; got %d entries", len(snap.Entries))
	}
}
