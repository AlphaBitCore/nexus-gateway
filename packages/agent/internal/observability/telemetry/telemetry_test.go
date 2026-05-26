package telemetry

import (
	"context"
	"log/slog"
	"testing"
)

func TestInitDisabled(t *testing.T) {
	provider, err := Init(context.Background(), Config{Enabled: false}, slog.Default())
	if err != nil {
		t.Fatalf("Init disabled returned error: %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown errored: %v", err)
	}
}
