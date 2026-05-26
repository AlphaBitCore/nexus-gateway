package capability

import (
	"sync/atomic"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// Snapshot is an immutable map from Model UUID to parsed ModelCapability.
// Produced from a Model row collection at config-load time and replaced
// atomically on shadow-pushed config changes.
type Snapshot struct {
	byID map[string]*ModelCapability
}

// NewSnapshot builds a Snapshot from a slice of store.Model rows.
// Models whose capabilityJson is nil or empty produce a ModelCapability
// with Embeddings == nil; callers should nil-check Embeddings before use.
func NewSnapshot(models []store.Model) *Snapshot {
	m := make(map[string]*ModelCapability, len(models))
	for i := range models {
		cap := ParseModelCapability(&models[i])
		if cap != nil {
			m[models[i].ID] = cap
		}
	}
	return &Snapshot{byID: m}
}

// Get returns the ModelCapability for the given model UUID, or nil if
// the model is not present in this snapshot.
func (s *Snapshot) Get(modelID string) *ModelCapability {
	if s == nil {
		return nil
	}
	return s.byID[modelID]
}

// Cache wraps an atomic.Pointer for hot-swappable Snapshots. Follows the
// same pattern as cache/passthrough and cache/gemini manager sets.
type Cache struct {
	ptr atomic.Pointer[Snapshot]
}

// NewCache returns a Cache pre-loaded with an empty snapshot so callers
// never observe a nil pointer on Load.
func NewCache() *Cache {
	c := &Cache{}
	c.ptr.Store(&Snapshot{byID: map[string]*ModelCapability{}})
	return c
}

// Replace atomically swaps in a new Snapshot. A nil argument is treated
// as an empty snapshot so the cache is never in a state where Load
// returns nil.
func (c *Cache) Replace(s *Snapshot) {
	if s == nil {
		s = &Snapshot{byID: map[string]*ModelCapability{}}
	}
	c.ptr.Store(s)
}

// Load returns the current Snapshot. Never returns nil.
func (c *Cache) Load() *Snapshot {
	return c.ptr.Load()
}
