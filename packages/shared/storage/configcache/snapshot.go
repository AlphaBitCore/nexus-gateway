package configcache

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
)

// SnapshotLoader returns the full set of items keyed by string ID.
type SnapshotLoader[T any] func(ctx context.Context) (map[string]T, error)

// SnapshotCache holds a whole-table snapshot in memory and serves lock-free
// reads via an atomic pointer. Reload swaps the entire map atomically.
type SnapshotCache[T any] struct {
	loader SnapshotLoader[T]
	snap   atomic.Pointer[map[string]T]
	log    *slog.Logger
	name   string

	onLoad func(name string, size int)
}

// SnapshotOption configures a SnapshotCache at construction time.
type SnapshotOption func(o *snapshotOptions)

type snapshotOptions struct {
	logger *slog.Logger
	name   string
	onLoad func(name string, size int)
}

// WithSnapshotLogger sets the slog.Logger used for cache events. If unset,
// slog.Default() is used.
func WithSnapshotLogger(l *slog.Logger) SnapshotOption {
	return func(o *snapshotOptions) { o.logger = l }
}

// WithSnapshotName sets a human-readable name surfaced in logs and metrics.
func WithSnapshotName(name string) SnapshotOption {
	return func(o *snapshotOptions) { o.name = name }
}

// WithSnapshotOnLoad registers a callback fired after each successful Load.
// Intended for Prometheus size gauges; called with the cache name and the
// number of items in the new snapshot.
func WithSnapshotOnLoad(fn func(name string, size int)) SnapshotOption {
	return func(o *snapshotOptions) { o.onLoad = fn }
}

// NewSnapshotCache builds a SnapshotCache. The returned cache has an empty
// snapshot until Load is called.
func NewSnapshotCache[T any](loader SnapshotLoader[T], opts ...SnapshotOption) *SnapshotCache[T] {
	if loader == nil {
		panic("configcache: loader must not be nil")
	}
	o := snapshotOptions{logger: slog.Default(), name: "snapshot"}
	for _, opt := range opts {
		opt(&o)
	}
	c := &SnapshotCache[T]{
		loader: loader,
		log:    o.logger,
		name:   o.name,
		onLoad: o.onLoad,
	}
	empty := map[string]T{}
	c.snap.Store(&empty)
	return c
}

// Load fetches the full set from the loader and atomically replaces the
// in-memory snapshot. Returns the loader error unchanged on failure; the
// previous snapshot stays in place.
func (c *SnapshotCache[T]) Load(ctx context.Context) error {
	if ctx == nil {
		return errors.New("configcache: nil context")
	}
	items, err := c.loader(ctx)
	if err != nil {
		c.log.Error("snapshot load failed", "cache", c.name, "error", err)
		loadFailures.WithLabelValues(c.name).Inc()
		return err
	}
	if items == nil {
		items = map[string]T{}
	}
	c.snap.Store(&items)
	lastLoadSuccess.WithLabelValues(c.name).SetToCurrentTime()
	c.log.Info("snapshot loaded", "cache", c.name, "size", len(items))
	if c.onLoad != nil {
		c.onLoad(c.name, len(items))
	}
	return nil
}

// Reload is an alias for Load. Used by invalidation handlers to make intent
// explicit at the call site.
func (c *SnapshotCache[T]) Reload(ctx context.Context) error {
	return c.Load(ctx)
}

// Get returns the item with the given ID and a presence flag.
func (c *SnapshotCache[T]) Get(id string) (T, bool) {
	m := c.snap.Load()
	if m == nil {
		var zero T
		return zero, false
	}
	v, ok := (*m)[id]
	return v, ok
}

// All returns the current snapshot map. Callers must treat it as read-only;
// SnapshotCache replaces the underlying map atomically on Load, so the
// returned reference remains a stable view of the snapshot at call time.
func (c *SnapshotCache[T]) All() map[string]T {
	m := c.snap.Load()
	if m == nil {
		return nil
	}
	return *m
}

// Size returns the number of items in the current snapshot.
func (c *SnapshotCache[T]) Size() int {
	m := c.snap.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// Name returns the cache name set via WithSnapshotName.
func (c *SnapshotCache[T]) Name() string {
	return c.name
}
