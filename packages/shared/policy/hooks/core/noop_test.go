package core

import (
	"context"
	"testing"
)

func TestNoopHook_ApproveRegardlessOfInput(t *testing.T) {
	cfg := &HookConfig{
		ID:               "hook-noop-1",
		ImplementationID: "noop",
		Name:             "test-noop",
		Stage:            "request",
		Config:           map[string]any{"anything": 42, "more": []any{"stuff"}},
	}
	h, err := NewNoop(cfg)
	if err != nil {
		t.Fatalf("NewNoop: %v", err)
	}

	res, err := h.Execute(context.Background(), &HookInput{
		RequestID:  "req-1",
		Stage:      "request",
		Normalized: PayloadFromTextSegments([]string{"hello"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("Decision = %q, want Approve", res.Decision)
	}
	if res.HookID != cfg.ID || res.HookName != cfg.Name || res.ImplementationID != cfg.ImplementationID {
		t.Errorf("metadata mismatch: %+v", res)
	}
}

// TestNoopHook_RegistryEntry is intentionally omitted from core/ because
// the global Registry is defined in the parent hooks package (aliases.go)
// to avoid an import cycle. The registry content is verified in hooks.TestDefaultRegistry_AllExpectedFactoriesPresent.
