package core

import (
	"context"
	"time"
)

// NoopHook always returns Approve. Useful as a placeholder in seeds, for
// configuration scaffolding, and for hook-pipeline smoke tests that need a
// hook instance that performs no logic.
type NoopHook struct {
	AnyEndpointAnyModality
	cfg *HookConfig
}

// NewNoop constructs a Noop hook. It ignores cfg.Config entirely.
func NewNoop(cfg *HookConfig) (Hook, error) {
	return &NoopHook{cfg: cfg}, nil
}

// Execute always returns a successful Approve result.
func (n *NoopHook) Execute(_ context.Context, _ *HookInput) (*HookResult, error) {
	start := time.Now()
	return &HookResult{
		HookID:           n.cfg.ID,
		ImplementationID: n.cfg.ImplementationID,
		HookName:         n.cfg.Name,
		Decision:         Approve,
		LatencyMs:        int(time.Since(start).Milliseconds()),
	}, nil
}
