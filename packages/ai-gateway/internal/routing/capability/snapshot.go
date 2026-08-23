package capability

import (
	"strings"
	"sync/atomic"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// Snapshot is an immutable map from Model UUID to parsed ModelCapability.
// Produced from a Model row collection at config-load time and replaced
// atomically on shadow-pushed config changes.
type Snapshot struct {
	byID map[string]*ModelCapability
	// described is the union of every InputModalities value, lower-cased.
	// Precomputed because Accepts consults it per recovery target.
	described map[string]bool
}

// NewSnapshot builds a Snapshot from a slice of store.Model rows.
// Models whose capabilityJson is nil or empty produce a ModelCapability
// with Embeddings == nil; callers should nil-check Embeddings before use.
func NewSnapshot(models []store.Model) *Snapshot {
	m := make(map[string]*ModelCapability, len(models))
	described := map[string]bool{}
	for i := range models {
		cap := ParseModelCapability(&models[i])
		if cap != nil {
			m[models[i].ID] = cap
			for _, mod := range cap.InputModalities {
				described[strings.ToLower(mod)] = true
			}
		}
	}
	return &Snapshot{byID: m, described: described}
}

// Describes tells two situations apart: a model omitting "audio" while others
// declare it really is saying it cannot take audio; but NO row declaring "file"
// or "video" — true of all 163 chat rows — is a fact about the catalog, and
// refusing on it rejects every such request including the ones that work.
//
// Needed by callers whose own candidate list is too small to answer it. The
// smart strategy's pool IS the catalog, so there an emptied dimension says the
// same thing; a recovery list of two admin-configured models does not.
func (s *Snapshot) Describes(modality string) bool {
	if s == nil {
		return false
	}
	return s.described[strings.ToLower(modality)]
}

// DescribedInputModalities is the union of every InputModalities value in the
// snapshot. Returns a copy so a caller cannot mutate the snapshot's own set.
func (s *Snapshot) DescribedInputModalities() map[string]bool {
	out := map[string]bool{}
	if s == nil {
		return out
	}
	for m := range s.described {
		out[m] = true
	}
	return out
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
