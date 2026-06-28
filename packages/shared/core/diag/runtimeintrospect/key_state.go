package runtimeintrospect

import (
	"context"
	"github.com/goccy/go-json"
	"sync"
)

// KeyStateRecorder captures the most recent acknowledged bytes per
// thingclient config_key and exposes a [Source] per key (named
// "config.<key>"). Services use it from their OnConfigChanged callback
// so the Runtime State tab on the admin UI surfaces a card for every
// consumed config_key — closing the gap where the admin pushed a
// template but had no way to verify the running service actually
// received and parsed it.
//
// The recorder stores the raw JSON bytes that OnConfigChanged echoed
// back into the reported map; if the apply also produced a deeper
// in-memory cache (e.g. payload_capture, hooks), that cache
// continues to be exposed under its own richer source name. The
// recorder fills the gap for keys that previously had no introspection
// view at all.
//
// Concurrency: safe for concurrent Record + Source reads.
type KeyStateRecorder struct {
	mu     sync.RWMutex
	states map[string]json.RawMessage
}

// NewKeyStateRecorder returns an empty recorder.
func NewKeyStateRecorder() *KeyStateRecorder {
	return &KeyStateRecorder{states: make(map[string]json.RawMessage)}
}

// Record stores the last bytes seen for key. Pass cs.State from the
// OnConfigChanged callback. Empty bytes clear the entry so the
// corresponding source returns nil — useful when a key is deleted
// upstream and the service drops its parsed copy.
func (r *KeyStateRecorder) Record(key string, raw json.RawMessage) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(raw) == 0 {
		delete(r.states, key)
		return
	}
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	r.states[key] = cp
}

// Source returns a [Source] for a single key, named "config.<key>".
// The Source emits the parsed JSON of the most recent bytes seen, or
// nil if Record has never been called for that key. Decode failures
// fall back to the raw string so the operator still sees the payload.
func (r *KeyStateRecorder) Source(key string) Source {
	return SourceFunc{
		SourceName: "config." + key,
		Fn: func(_ context.Context) (any, error) {
			r.mu.RLock()
			raw, ok := r.states[key]
			r.mu.RUnlock()
			if !ok || len(raw) == 0 {
				return nil, nil
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				// Best-effort introspection: when the stored bytes are
				// not valid JSON, return them verbatim as a string so
				// operators can still see what the consumer wrote. The
				// unmarshal failure itself is not propagated.
				return string(raw), nil //nolint:nilerr // intentional fallback to raw bytes
			}
			return v, nil
		},
	}
}

// RegisterAll registers a "config.<key>" Source for every key in keys.
// Convenience wrapper around r.Source(key) for the common case where
// the caller knows the consumed-key list up front.
func (r *KeyStateRecorder) RegisterAll(reg *Registry, keys []string) {
	if r == nil || reg == nil {
		return
	}
	for _, k := range keys {
		reg.Register(r.Source(k))
	}
}
