package store

import (
	"context"
	"sync"
)

// CatBLoader aggregates the authoritative state for a single Cat B
// (thingType, configKey) pair by reading the Control Plane's business
// tables from Hub's main DB pool.
//
// Return contract:
//   - state is an opaque value that will be JSON-marshalled into the
//     handler response under the "state" key. Loaders SHOULD return a
//     shape that matches the consuming Thing's ShadowApplier exactly
//     (see docs/developers/specs/e3/e3-s5-config-sync-remediation.md for agent shapes).
//   - state = map[string]any{} (an empty JSON object) is the "no-op"
//     signal: agent ShadowAppliers treat "{}" as "leave local defaults
//     intact". Loaders MUST use this when scope is empty (e.g. an agent
//     with no DeviceGroup memberships) rather than returning an
//     authoritative empty list, which would wipe local yaml defaults.
//   - version is a monotonically-useful integer derived from row
//     timestamps (usually max(updated_at::unix) or createdAt::unix).
//     0 is valid and means "no rows / no timestamp".
//   - err non-nil MUST surface a 500 to the caller — the handler
//     intentionally does not fall back to Cat A template state on
//     loader errors, because a silent fallback would replay an empty
//     payload to the Thing on a transient DB blip.
//
// thingID is always passed so per-Thing scoping (e.g. DeviceGroup
// membership for policy_rules) can plug in without changing the
// interface. Loaders that don't scope today ignore it.
type CatBLoader interface {
	Load(ctx context.Context, thingID string) (state any, version int64, err error)
}

// CatBRegistry is a composable (thingType, configKey) -> loader lookup.
// Intentionally nil-safe: a (*CatBRegistry)(nil) pointer allows Lookup
// to return (nil, false) without panicking so the handler can fall
// through to the legacy thing_config_template.state path unchanged.
type CatBRegistry struct {
	mu      sync.RWMutex
	loaders map[string]CatBLoader
}

// NewCatBRegistry constructs an empty registry.
func NewCatBRegistry() *CatBRegistry {
	return &CatBRegistry{loaders: make(map[string]CatBLoader)}
}

// Register stores a loader under the (thingType, configKey) key. Later
// calls with the same pair replace the earlier loader (last write wins);
// registration is expected to happen once at process startup so the
// mutex exists purely to keep the data race detector happy when tests
// register concurrently.
func (r *CatBRegistry) Register(thingType, configKey string, l CatBLoader) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaders == nil {
		r.loaders = make(map[string]CatBLoader)
	}
	r.loaders[registryKey(thingType, configKey)] = l
}

// Lookup returns (loader, true) when a loader is registered for the
// pair, or (nil, false) so the caller can fall back to the
// thing_config_template.state path. Safe to call on a nil receiver.
func (r *CatBRegistry) Lookup(thingType, configKey string) (CatBLoader, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.loaders[registryKey(thingType, configKey)]
	return l, ok
}

func registryKey(thingType, configKey string) string {
	return thingType + "|" + configKey
}
