package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// CacheCategory represents a config category.
type CacheCategory string

const (
	// CategoryHooks is the hooks config category.
	CategoryHooks CacheCategory = "hooks"
	// CategoryInterceptionDomains is the interception domain/path config category (V2 §2.9).
	CategoryInterceptionDomains CacheCategory = "interceptionDomains"
	// CategoryAllowlists is the allowlists config category.
	CategoryAllowlists CacheCategory = "allowlists"
	// CategoryObservability is the observability config category (OTEL tracing).
	CategoryObservability CacheCategory = "observability"
)

// CacheEntry holds cached data with expiry tracking.
type CacheEntry struct {
	Data        interface{}
	LastRefresh time.Time
	Valid       bool // false after invalidation, until next successful load
}

// Manager manages in-memory config caches with invalidation support.
// It uses a read-write mutex so concurrent cache-hit reads do not block each
// other; writes (invalidation or reload) acquire exclusive access.
type Manager struct {
	mu        sync.RWMutex
	caches    map[CacheCategory]*CacheEntry
	ttl       time.Duration
	logger    *slog.Logger
	loadFuncs map[CacheCategory]func(ctx context.Context) (interface{}, error)
}

// NewManager creates a cache manager with the given TTL.
// A zero or negative TTL disables time-based expiry (only explicit invalidation
// triggers reload).
func NewManager(ttl time.Duration, logger *slog.Logger) *Manager {
	return &Manager{
		caches:    make(map[CacheCategory]*CacheEntry),
		ttl:       ttl,
		logger:    logger,
		loadFuncs: make(map[CacheCategory]func(ctx context.Context) (interface{}, error)),
	}
}

// RegisterLoader sets the DB load function for a category. It must be called
// before any Get for that category. It is not safe to call concurrently with
// Get on the same category.
func (m *Manager) RegisterLoader(cat CacheCategory, loadFn func(ctx context.Context) (interface{}, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadFuncs[cat] = loadFn
	// Initialize cache entry as invalid so first Get triggers a load.
	if _, ok := m.caches[cat]; !ok {
		m.caches[cat] = &CacheEntry{Valid: false}
	}
}

// Get retrieves cached data for the given category. If the entry is invalid
// (explicitly invalidated or not yet loaded) or the TTL has expired, the
// registered load function is called to refresh the data.
//
// On load error the stale data (if any) is returned alongside the error, and
// the entry remains invalid so the next call retries.
func (m *Manager) Get(ctx context.Context, cat CacheCategory) (interface{}, error) {
	// Fast path: read lock, check validity and TTL.
	m.mu.RLock()
	entry, exists := m.caches[cat]
	if exists && entry.Valid && !m.isExpired(entry) {
		data := entry.Data
		m.mu.RUnlock()
		if CacheHits != nil {
			CacheHits.With(string(cat)).Inc()
		}
		m.updateStalenessMetric(cat, entry.LastRefresh)
		return data, nil
	}
	m.mu.RUnlock()

	// Slow path: acquire write lock and reload.
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check: another goroutine may have reloaded while we waited.
	entry, exists = m.caches[cat]
	if exists && entry.Valid && !m.isExpired(entry) {
		if CacheHits != nil {
			CacheHits.With(string(cat)).Inc()
		}
		m.updateStalenessMetric(cat, entry.LastRefresh)
		return entry.Data, nil
	}

	// Reload via loadFunc.
	if CacheMisses != nil {
		CacheMisses.With(string(cat)).Inc()
	}

	loadFn, ok := m.loadFuncs[cat]
	if !ok {
		return nil, fmt.Errorf("configcache: no loader registered for category %q", cat)
	}

	data, err := loadFn(ctx)
	if err != nil {
		m.logger.Warn("config cache reload failed, serving stale data",
			slog.String("category", string(cat)),
			slog.String("error", err.Error()),
		)
		// Return stale data if available.
		if exists && entry.Data != nil {
			return entry.Data, err
		}
		return nil, err
	}

	// Store refreshed data.
	now := time.Now()
	if !exists {
		entry = &CacheEntry{}
		m.caches[cat] = entry
	}
	entry.Data = data
	entry.LastRefresh = now
	entry.Valid = true

	if LastRefresh != nil {
		LastRefresh.With(string(cat)).Set(float64(now.Unix()))
	}
	if Staleness != nil {
		Staleness.With(string(cat)).Set(0)
	}

	m.logger.Info("config cache refreshed",
		slog.String("category", string(cat)),
	)
	return data, nil
}

// Invalidate marks a single category as invalid. The next Get call will
// trigger a reload from the registered load function.
func (m *Manager) Invalidate(cat CacheCategory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.caches[cat]; ok {
		entry.Valid = false
		m.logger.Info("config cache invalidated",
			slog.String("category", string(cat)),
		)
	}
}

// InvalidateAll marks all categories as invalid.
func (m *Manager) InvalidateAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for cat, entry := range m.caches {
		entry.Valid = false
		m.logger.Info("config cache invalidated",
			slog.String("category", string(cat)),
		)
	}
}

// Staleness returns seconds since last successful refresh for a category.
// Returns -1 if no refresh has occurred.
func (m *Manager) StalenessSeconds(cat CacheCategory) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.caches[cat]
	if !ok || entry.LastRefresh.IsZero() {
		return -1
	}
	return time.Since(entry.LastRefresh).Seconds()
}

// isExpired checks whether the entry's TTL has elapsed. Must be called under
// at least a read lock. A zero TTL means no time-based expiry.
func (m *Manager) isExpired(entry *CacheEntry) bool {
	if m.ttl <= 0 {
		return false
	}
	return time.Since(entry.LastRefresh) > m.ttl
}

// updateStalenessMetric publishes the current staleness to the Prometheus gauge.
func (m *Manager) updateStalenessMetric(cat CacheCategory, lastRefresh time.Time) {
	if lastRefresh.IsZero() || Staleness == nil {
		return
	}
	Staleness.With(string(cat)).Set(time.Since(lastRefresh).Seconds())
}
