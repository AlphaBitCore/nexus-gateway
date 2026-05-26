// Package hooks provides the built-in compliance hook implementations
// shared by all three data plane services.
package core

import (
	"fmt"
	"sync"
)

// HookRegistry is a goroutine-safe, freezable registry of hook factories.
// After Freeze() is called, Register panics to prevent runtime mutation.
// Both registration and lookup are guarded by mu so callers that build
// custom registries (without going through the package init() happens-before)
// stay race-free under -race.
type HookRegistry struct {
	mu        sync.RWMutex
	factories map[string]HookFactory
	frozen    bool
}

// NewHookRegistry creates an empty registry. Call Register to add factories,
// then Freeze when initialization is complete.
func NewHookRegistry() *HookRegistry {
	return &HookRegistry{factories: make(map[string]HookFactory)}
}

// Register adds a factory under the given implementation ID. Panics if the
// registry is frozen or the ID is already registered.
func (r *HookRegistry) Register(implID string, factory HookFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Sprintf("hooks: Register(%q) called on frozen registry", implID))
	}
	if _, exists := r.factories[implID]; exists {
		panic(fmt.Sprintf("hooks: duplicate registration for %q", implID))
	}
	r.factories[implID] = factory
}

// Replace overrides an existing factory in the registry. Unlike Register,
// Replace does NOT panic on a duplicate ID — it overwrites. Panics only
// when the registry is frozen.
//
// Used by data-plane services that want to swap a default shared-hooks
// factory for a service-specific variant: e.g. ai-gateway swaps
// `webhook-forward` with a version that uses a shared http.Client pool
// for higher throughput. agent / compliance-proxy keep the default
// per-hook-client behaviour.
func (r *HookRegistry) Replace(implID string, factory HookFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Sprintf("hooks: Replace(%q) called on frozen registry", implID))
	}
	r.factories[implID] = factory
}

// Freeze prevents further registration. Must be called before concurrent use.
func (r *HookRegistry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Get returns the factory for the given implementation ID, or nil if not found.
func (r *HookRegistry) Get(implID string) HookFactory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.factories[implID]
}

// Clone creates a new unfrozen registry with all factories from this one.
// Use this to extend a frozen registry with service-specific hooks.
func (r *HookRegistry) Clone() *HookRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := NewHookRegistry()
	for id, factory := range r.factories {
		clone.factories[id] = factory
	}
	return clone
}

// All returns a snapshot of all registered factory IDs (for diagnostics).
func (r *HookRegistry) All() map[string]HookFactory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]HookFactory, len(r.factories))
	for k, v := range r.factories {
		out[k] = v
	}
	return out
}

// The global Registry variable is initialized in the builtins package
// (hooks/builtins/builtins.go) because sub-packages (validators, access,
// ratelimit, webhook) import core — defining it here would create an import
// cycle. builtins.Registry is the canonical consumer entry point.
