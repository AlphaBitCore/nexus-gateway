package wiring

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
)

func TestInitDB_EmptyURL_ReturnsNilDB(t *testing.T) {
	cfg := &config.Config{} // Database.URL == ""
	db, closer, err := InitDB(context.Background(), cfg, silentLogger())
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if db != nil {
		t.Error("expected nil *store.DB when URL is empty")
	}
	// closer must be callable without panic.
	closer()
}

func TestInitDB_InvalidURL_ReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database.URL = "postgres://127.0.0.1:1/nonexistent" // nothing listening
	_, closer, err := InitDB(context.Background(), cfg, silentLogger())
	defer closer()
	if err == nil {
		t.Fatal("expected error for invalid/unreachable DB URL, got nil")
	}
}
