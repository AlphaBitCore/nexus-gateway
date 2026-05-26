// Package cachelayer is the data-plane configuration cache wired into
// ai-gateway's hot path. It exposes lookup methods that satisfy the
// existing handler/provtarget interfaces (ProviderStore, ModelStore,
// CredentialStore, ModelLookup, CredentialLookup, VKLookup) but reads
// from in-memory caches instead of PostgreSQL.
//
// Not to be confused with `cache/` — that package caches *response
// bodies* in Redis. This package caches *config rows* (Provider, Model,
// Credential, VirtualKey, …) in process memory. Both are "caches" but
// for entirely different domains.
//
// SnapshotCache holds the small full tables (Provider, Model,
// Credential). KeyCache holds the larger per-key tables (VirtualKey,
// User, Org, Project — the latter three added by future stories on top
// of this scaffolding).
//
// Snapshots are loaded eagerly at Start and replaced atomically on
// Reload. KeyCache entries populate lazily on first Get and expire by
// TTL (with explicit invalidate via Hub thingclient push).
//
// See `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` for the full
// multi-tier picture (response cache + config cache + stream cache +
// Gemini cached content + provider builtins).
package cachelayer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configcache"
)

// PgxPool is the minimum pgx surface the snapshot loaders need. The
// concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the seam already used by
// `packages/ai-gateway/internal/store.PgxPool` and the canonical
// `packages/nexus-hub/internal/siem/bridge.go` pattern.
type PgxPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Layer holds every configuration cache served to the ai-gateway hot
// path. Construct once at startup, call Start to perform initial loads,
// then pass the lookup-method receivers (Providers/Models/Credentials/
// VirtualKeys) into the call sites that take *store.DB or
// *credentials.Manager.
type Layer struct {
	db   *store.DB
	pool PgxPool // interface seam — defaults to db.Pool; overridable in tests via NewWithPool
	log  *slog.Logger

	providers   *configcache.SnapshotCache[store.Provider]
	models      *configcache.SnapshotCache[store.Model]
	credentials *configcache.SnapshotCache[store.Credential]
	vkeys       *configcache.KeyCache[string, *store.VirtualKey]

	// Secondary indices computed alongside snapshot loads. Replaced
	// atomically; readers never see a torn state.
	modelsByCode               atomic.Pointer[map[string]store.Model]
	credentialsByProviderFirst atomic.Pointer[map[string]store.Credential]

	// invalidationCount tracks how many entries the cache evicted on
	// Reload. Surfaced via Stats for ops; not exposed in metrics yet.
	invalidationCount atomic.Uint64

	// LookupCachePricing reads from modelsByCode (above).

	// Metrics hook slots populated by Metrics.Bind. All are nil-safe;
	// nil hooks are skipped at call time.
	vkOnHit          func()
	vkOnMiss         func()
	vkOnInvalidate   func(removed int)
	snapshotOnReload func(name string)
}

// Config configures the Layer.
type Config struct {
	// VKCapacity is the LRU upper bound for the virtual-key cache.
	// Defaults to 10000 if zero.
	VKCapacity int
	// VKTTL is the per-entry TTL for the virtual-key cache. Defaults
	// to 30 seconds if zero. Bounds revoke latency in the absence of
	// an explicit Hub push.
	VKTTL time.Duration
	// Metrics, when non-nil, wires Prometheus instrumentation into
	// every underlying cache at construction time. Pass the result of
	// NewMetrics(namespace).
	Metrics *Metrics
}

// New constructs a Layer. The caches are empty until Start is called.
func New(db *store.DB, log *slog.Logger, cfg Config) (*Layer, error) {
	if db == nil {
		return nil, errors.New("cachelayer: db must not be nil")
	}
	return newLayer(db, db.Pool, log, cfg)
}

// NewWithPool is the test-only constructor: caller supplies the
// PgxPool seam directly (typically a pgxmock pool) so loader-level
// SQL can be exercised without a live Postgres. *store.DB is still
// required for the methods that compose store-level helpers
// (GetVirtualKeyByHash, GetEnabledRoutingRules, InvalidateRuleCache);
// pass a `store.NewWithPgxPool(pool)` so those round-trips also go
// through the same mock.
func NewWithPool(db *store.DB, pool PgxPool, log *slog.Logger, cfg Config) (*Layer, error) {
	if db == nil {
		return nil, errors.New("cachelayer: db must not be nil")
	}
	if pool == nil {
		return nil, errors.New("cachelayer: pool must not be nil")
	}
	return newLayer(db, pool, log, cfg)
}

func newLayer(db *store.DB, pool PgxPool, log *slog.Logger, cfg Config) (*Layer, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.VKCapacity == 0 {
		cfg.VKCapacity = 10000
	}
	if cfg.VKTTL == 0 {
		cfg.VKTTL = 30 * time.Second
	}

	l := &Layer{db: db, pool: pool, log: log}
	if cfg.Metrics != nil {
		cfg.Metrics.bindLayer(l)
	}

	l.providers = configcache.NewSnapshotCache(l.loadProviders,
		configcache.WithSnapshotName("providers"),
		configcache.WithSnapshotLogger(log),
	)
	l.models = configcache.NewSnapshotCache(l.loadModels,
		configcache.WithSnapshotName("models"),
		configcache.WithSnapshotLogger(log),
	)
	l.credentials = configcache.NewSnapshotCache(l.loadCredentials,
		configcache.WithSnapshotName("credentials"),
		configcache.WithSnapshotLogger(log),
	)
	vkOpts := []configcache.KeyOption{
		configcache.WithKeyName("virtual_keys"),
		configcache.WithKeyLogger(log),
	}
	if l.vkOnHit != nil {
		vkOpts = append(vkOpts, configcache.WithKeyOnHit(func(string) { l.vkOnHit() }))
	}
	if l.vkOnMiss != nil {
		vkOpts = append(vkOpts, configcache.WithKeyOnMiss(func(string) { l.vkOnMiss() }))
	}
	vk, err := configcache.NewKeyCache[string, *store.VirtualKey](
		l.loadVirtualKey,
		cfg.VKCapacity,
		cfg.VKTTL,
		vkOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("cachelayer: build vk cache: %w", err)
	}
	l.vkeys = vk
	return l, nil
}

// Start performs the initial eager loads of all snapshot caches. Each
// loader runs once; failures are logged but do not abort startup, so a
// transient DB blip does not prevent the gateway from coming up (the
// snapshot will repopulate on the next Reload triggered by Hub push).
func (l *Layer) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	type loadResult struct {
		name string
		err  error
	}
	results := make(chan loadResult, 4)

	wg.Add(3)
	go func() { defer wg.Done(); results <- loadResult{"providers", l.providers.Load(ctx)} }()
	go func() { defer wg.Done(); results <- loadResult{"models", l.models.Load(ctx)} }()
	go func() { defer wg.Done(); results <- loadResult{"credentials", l.credentials.Load(ctx)} }()
	// Model rows are the single source of truth for input/output/cache prices.

	wg.Wait()
	close(results)

	var errs []string
	for r := range results {
		if r.err != nil {
			l.log.Error("cachelayer: initial load failed", "snapshot", r.name, "error", r.err)
			errs = append(errs, fmt.Sprintf("%s: %v", r.name, r.err))
		}
	}
	// Refresh size gauges so initial scrapes are non-zero.
	if l.snapshotOnReload != nil {
		l.snapshotOnReload("providers")
		l.snapshotOnReload("models")
		l.snapshotOnReload("credentials")
	}
	if len(errs) > 0 {
		return fmt.Errorf("cachelayer: start: %s", strings.Join(errs, "; "))
	}
	l.log.Info("cachelayer: initial snapshot loads complete",
		"providers", l.providers.Size(),
		"models", l.models.Size(),
		"credentials", l.credentials.Size(),
	)
	return nil
}

// ReloadProviders refetches the Provider table snapshot.
func (l *Layer) ReloadProviders(ctx context.Context) error {
	err := l.providers.Reload(ctx)
	if err == nil && l.snapshotOnReload != nil {
		l.snapshotOnReload("providers")
	}
	return err
}

// ReloadModels refetches the Model table snapshot.
func (l *Layer) ReloadModels(ctx context.Context) error {
	err := l.models.Reload(ctx)
	if err == nil && l.snapshotOnReload != nil {
		l.snapshotOnReload("models")
	}
	return err
}

// ReloadCredentials refetches the Credential table snapshot.
func (l *Layer) ReloadCredentials(ctx context.Context) error {
	err := l.credentials.Reload(ctx)
	if err == nil && l.snapshotOnReload != nil {
		l.snapshotOnReload("credentials")
	}
	return err
}

// InvalidateVirtualKeys evicts the named hashes from the VK cache.
// Missing hashes are silently ignored. Returns the number of entries
// actually removed.
func (l *Layer) InvalidateVirtualKeys(hashes ...string) int {
	n := l.vkeys.Invalidate(hashes...)
	l.invalidationCount.Add(uint64(n))
	if l.vkOnInvalidate != nil && n > 0 {
		l.vkOnInvalidate(n)
	}
	return n
}

// PurgeVirtualKeys clears the entire VK cache. Used when the Hub sends
// a reset signal (e.g. after a bulk VK migration).
func (l *Layer) PurgeVirtualKeys() {
	n := l.vkeys.Size()
	l.vkeys.Purge()
	if l.vkOnInvalidate != nil && n > 0 {
		l.vkOnInvalidate(n)
	}
}

// Stats reports basic cache sizes and counters for ops introspection.
type Stats struct {
	ProvidersSize    int
	ModelsSize       int
	CredentialsSize  int
	VirtualKeysSize  int
	VKHits           uint64
	VKMisses         uint64
	VKInvalidations  uint64
	TotalInvalidates uint64
}

// Stats returns a snapshot of the current counters.
func (l *Layer) Stats() Stats {
	hits, misses, inv := l.vkeys.Stats()
	return Stats{
		ProvidersSize:    l.providers.Size(),
		ModelsSize:       l.models.Size(),
		CredentialsSize:  l.credentials.Size(),
		VirtualKeysSize:  l.vkeys.Size(),
		VKHits:           hits,
		VKMisses:         misses,
		VKInvalidations:  inv,
		TotalInvalidates: l.invalidationCount.Load(),
	}
}
