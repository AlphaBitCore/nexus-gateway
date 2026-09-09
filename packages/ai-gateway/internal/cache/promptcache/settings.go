// Package promptcache holds the AI Gateway's live view of the operator's
// prompt-cache configuration — the `cache` shadow key's three-tier blob — and
// answers the one question the request path asks of it: does the provider this
// request is going to want its upstream prompt cache turned on.
//
// It is a HOLDER, not a projection. The previous design projected the blob
// against a snapshot of the provider list at the moment the `cache` key was
// applied, and cached the result per provider UUID. That has two failure modes
// and both are silent: a provider created after the last cache push had no
// entry and never got markers, and the config loader applies shadow keys by
// ranging a Go map, so a cold start that reached `cache` before `providers`
// projected against an empty provider list and left markers off for the whole
// process lifetime — with the admin UI still showing the toggle on.
//
// Holding the blob and resolving per request removes the class rather than
// patching it: there is no precomputed provider list to go stale, so key
// arrival order stops mattering.
package promptcache

import (
	"sync/atomic"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

// Settings is safe for concurrent use and cheap to read: one atomic load per
// request plus two map lookups. The zero value is usable and answers false for
// everything, which is the correct answer before any config has arrived.
type Settings struct {
	blob atomic.Pointer[cacheconfig.CacheConfigBlob]
}

// New returns an empty Settings. Callers may equally use the zero value; this
// exists so wiring reads as a construction.
func New() *Settings { return &Settings{} }

// SetConfig replaces the held blob. Called from the `cache` shadow-key
// applier on every push; in-flight reads finish against the previous blob.
func (s *Settings) SetConfig(blob cacheconfig.CacheConfigBlob) {
	if s == nil {
		return
	}
	s.blob.Store(&blob)
}

// MarkersEnabled reports whether this provider wants prompt-cache markers on
// requests the gateway sends it, resolving Tier 3 → Tier 2 → code default.
//
// A nil receiver answers false so a call site can hold an optional dependency
// without a nil check, matching how the gateway's other optional runtime
// dependencies behave.
func (s *Settings) MarkersEnabled(providerID, adapterType string) bool {
	if s == nil {
		return false
	}
	blob := s.blob.Load()
	if blob == nil {
		return false
	}
	return cacheconfig.MarkerInjectEnabledFor(*blob, providerID, adapterType)
}

// BoundaryEnabled reports whether this provider also wants the SECOND, explicit
// breakpoint one turn behind the automatic one. Meaningful only when
// MarkersEnabled is true; the codec checks that first.
func (s *Settings) BoundaryEnabled(providerID, adapterType string) bool {
	if s == nil {
		return false
	}
	blob := s.blob.Load()
	if blob == nil {
		return false
	}
	return cacheconfig.MarkerBoundary3EnabledFor(*blob, providerID, adapterType)
}
