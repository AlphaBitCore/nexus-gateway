package runtimeintrospect

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Source is a named contributor to a service's runtime snapshot.
// Convention for Name(): "config.<key>" for thingclient config_keys,
// "cache.<category>" for configcache categories, "runtime.<area>"
// for ad-hoc state. Snapshot() must be O(in-memory state) — no DB
// or network calls — and must redact secrets.
type Source interface {
	Name() string
	Snapshot(ctx context.Context) (any, error)
}

type Meta struct {
	Service          string    `json:"service"`
	ThingID          string    `json:"thing_id"`
	ThingVersion     string    `json:"thing_version"`
	ProcessStartedAt time.Time `json:"process_started_at"`
}

type SourceResult struct {
	OK    bool   `json:"ok"`
	Value any    `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type Response struct {
	Meta            Meta                    `json:"meta"`
	SnapshotTakenAt time.Time               `json:"snapshot_taken_at"`
	Sources         map[string]SourceResult `json:"sources"`
}

type Registry struct {
	meta    Meta
	mu      sync.RWMutex
	sources map[string]Source
}

func New(service, thingID, thingVersion string) *Registry {
	return &Registry{
		meta: Meta{
			Service:          service,
			ThingID:          thingID,
			ThingVersion:     thingVersion,
			ProcessStartedAt: time.Now().UTC(),
		},
		sources: make(map[string]Source),
	}
}

// Register adds or replaces a Source. Re-registration with the same Name
// overwrites — allowing hot-reloading components to refresh their entry.
func (r *Registry) Register(s Source) {
	if s == nil {
		return
	}
	name := s.Name()
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources[name] = s
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.sources))
	for n := range r.sources {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Snapshot collects all Sources. A panicking or erroring Source produces
// SourceResult{OK: false, Error: ...}; other Sources keep serving.
func (r *Registry) Snapshot(ctx context.Context) Response {
	r.mu.RLock()
	sources := make([]Source, 0, len(r.sources))
	for _, s := range r.sources {
		sources = append(sources, s)
	}
	r.mu.RUnlock()

	results := make(map[string]SourceResult, len(sources))
	for _, s := range sources {
		results[s.Name()] = collect(ctx, s)
	}
	return Response{
		Meta:            r.meta,
		SnapshotTakenAt: time.Now().UTC(),
		Sources:         results,
	}
}

func collect(ctx context.Context, s Source) (result SourceResult) {
	defer func() {
		if rec := recover(); rec != nil {
			result = SourceResult{OK: false, Error: fmt.Sprintf("panic: %v", rec)}
		}
	}()
	val, err := s.Snapshot(ctx)
	if err != nil {
		return SourceResult{OK: false, Error: err.Error()}
	}
	return SourceResult{OK: true, Value: val}
}

// SourceFunc adapts a plain function into a Source.
type SourceFunc struct {
	SourceName string
	Fn         func(ctx context.Context) (any, error)
}

func (s SourceFunc) Name() string { return s.SourceName }

func (s SourceFunc) Snapshot(ctx context.Context) (any, error) {
	if s.Fn == nil {
		return nil, errors.New("nil snapshot func")
	}
	return s.Fn(ctx)
}
