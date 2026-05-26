package wiring

import (
	"context"
	"testing"
)

func TestLoadOtelConfig_NilDBReturnsDefault(t *testing.T) {
	cfg := LoadOtelConfig(context.Background(), nil)
	if cfg.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("got service name %q; want nexus-compliance-proxy", cfg.ServiceName)
	}
}

func TestLoadOtelConfig_NilDBDoesNotPanic(t *testing.T) {
	// Ensure no panic even with nil db and a cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := LoadOtelConfig(ctx, nil)
	if cfg.ServiceName == "" {
		t.Error("expected non-empty service name from nil-db path")
	}
}
