package wiring

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
)

// TestInitDB_emptyDSN verifies that an empty DSN returns (nil, nil) — degraded
// mode without a database.
func TestInitDB_emptyDSN(t *testing.T) {
	db, err := InitDB(context.Background(), config.DatabaseConfig{})
	if err != nil {
		t.Fatalf("unexpected error for empty DSN: %v", err)
	}
	if db != nil {
		t.Fatal("expected nil DB for empty DSN")
	}
}
