package policies

import (
	"context"
	"encoding/json"
	"sync"
)

// SnapshotCache captures the raw JSON state each Cat B applier was invoked
// with. The Policies page's Build() needs to render rows from the
// hooks / interception_domains / payload_capture / policy_rules
// payloads — but those keys flow into the agent via HTTP pull (manager.go
// RefreshPullKeys / Cat B dispatch), not via thingclient's desired cache,
// so reading them off the cache yields empty rows. The cache here mirrors
// the raw bytes the applier accepted so Build() has an authoritative
// source independent of which transport (WS push vs HTTP pull) delivered
// the payload.
//
// Concurrency: Set is called from the configsync dispatch goroutine
// (single-threaded today, but RW-safe regardless); Get is called from
// the IPC handler goroutine that serves GET_APPLIED_CONFIG. The mutex
// guards the map and the byte slices are treated read-only by callers.
type SnapshotCache struct {
	mu  sync.RWMutex
	raw map[string]json.RawMessage
}

// NewSnapshotCache constructs an empty cache.
func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{raw: make(map[string]json.RawMessage)}
}

// Set records the raw payload most recently applied for a config key.
// Passing a zero-length payload is treated as a clear; the cache then
// behaves as if no apply ever happened for that key.
func (c *SnapshotCache) Set(key string, payload json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(payload) == 0 {
		delete(c.raw, key)
		return
	}
	cp := make(json.RawMessage, len(payload))
	copy(cp, payload)
	c.raw[key] = cp
}

// Get returns the cached raw payload for a key, or nil if nothing has
// been applied yet.
func (c *SnapshotCache) Get(key string) json.RawMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.raw[key]
	if !ok {
		return nil
	}
	out := make(json.RawMessage, len(v))
	copy(out, v)
	return out
}

// TeeApplier wraps an existing ShadowApplier so a successful apply also
// records the raw payload in the snapshot cache. Wired around each Cat B
// applier at agent startup so the Policies page can read the authoritative
// post-pull state regardless of where the daemon's WS push cycle is.
type TeeApplier struct {
	Inner  ShadowApplier
	Cache  *SnapshotCache
	CfgKey string
}

// ApplyShadowState delegates to the inner applier and, on success, stores
// the raw bytes in the cache under CfgKey. Failures are propagated
// unchanged; the cache is NOT updated on failure so a previously-applied
// payload remains visible to the UI rather than being silently replaced
// with broken data.
func (t TeeApplier) ApplyShadowState(ctx context.Context, raw json.RawMessage) error {
	if err := t.Inner.ApplyShadowState(ctx, raw); err != nil {
		return err
	}
	if t.Cache != nil {
		t.Cache.Set(t.CfgKey, raw)
	}
	return nil
}

// ShadowApplier is the minimal interface TeeApplier wraps. Matches
// shadow.ShadowApplier shape without importing the configsync
// package (avoids a circular dependency since configsync is wired by
// main.go which also wires the policies builder).
type ShadowApplier interface {
	ApplyShadowState(ctx context.Context, raw json.RawMessage) error
}
