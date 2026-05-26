// Package configsync exposes the shared types that the agent's shadow
// pipeline uses to talk to its subsystems.
//
// Historically this package also owned the dispatcher (shadow.Manager,
// NewManager, ManagerConfig, ApplyDesired, RefreshPullKeys, pullConfig).
// That dispatcher was retired when the agent migrated onto
// shared/transport/configloader.Loader (per R3 of the architecture
// refactor program); the dispatch table now lives in
// cmd/agent/configdispatch.go and the HTTP-pull semantics live inside
// the shared package's Loader. What remains in configsync/ is the
// non-dispatcher surface still consumed by agent subsystems:
//
//   - ShadowApplier / AdapterFunc — the per-subsystem applier interface
//     used by both the new configdispatch.go wiring and by helpers
//     like policies.TeeApplier.
//   - ConfigState — a parallel-shape wrapper kept for backward
//     compatibility with code that hand-rolled this shape; new code
//     should use thingclient.ConfigState directly.
//   - ConfigSnapshot + InterceptionDomainDTO + InterceptionPathDTO
//     (snapshot.go) + dto_to_domainpolicy.go + cache.go + offline.go —
//     all still relevant.
package shadow

import (
	"context"
	"encoding/json"
)

// ConfigState mirrors thingclient.ConfigState for shadow config
// delivery. Retained on the legacy path; new code should use
// thingclient.ConfigState directly.
type ConfigState struct {
	State   json.RawMessage `json:"state"`
	Version int64           `json:"version"`
}

// ShadowApplier is the unified interface for applying a shadow-pushed
// config state to a subsystem. Used both by the configdispatch.go
// wiring (which wraps each applier as a rawApply closure) and by
// helpers like policies.TeeApplier. Implementers MUST treat an empty
// / null state as a no-op rather than as "clear everything" —
// otherwise an initial shadow tick before Hub has aggregated Cat B
// data (P0-C) would wipe the local-yaml-loaded defaults.
type ShadowApplier interface {
	ApplyShadowState(ctx context.Context, raw json.RawMessage) error
}

// AdapterFunc lets callers pass a bound method as a ShadowApplier
// without declaring a wrapper type. Used in cmd/agent/main.go to plug
// e.g. AgentPipeline.ApplyDomainsShadowState into a Cat B applier
// slot.
type AdapterFunc func(ctx context.Context, raw json.RawMessage) error

// ApplyShadowState makes AdapterFunc satisfy ShadowApplier.
func (f AdapterFunc) ApplyShadowState(ctx context.Context, raw json.RawMessage) error {
	return f(ctx, raw)
}
