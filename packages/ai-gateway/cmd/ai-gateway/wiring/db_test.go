package wiring

import (
	"context"
	"testing"
)

// TestInitDB_emptyDSN verifies that an empty DSN returns (nil, nil) — degraded
// mode without a database.
func TestInitDB_emptyDSN(t *testing.T) {
	db, err := InitDB(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error for empty DSN: %v", err)
	}
	if db != nil {
		t.Fatal("expected nil DB for empty DSN")
	}
}
