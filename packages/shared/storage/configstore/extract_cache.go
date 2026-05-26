package configstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExtractCacheConfigRow mirrors the extract_cache_config table row.
//
// Fleet-wide singleton (id = "singleton"). Carries the runtime config for the
// L1 exact-match response cache (cache/core/), and the apply_freshness_rules
// gate that decides whether classifyCachePreLookup honours a freshness
// detector match by skipping BOTH L1 and L2.
//
// Admin-managed so operators can disable the cache or stop freshness rules
// from firing without a service restart.
type ExtractCacheConfigRow struct {
	ID                  string    `json:"id"`
	Enabled             bool      `json:"enabled"`
	TTLSeconds          int       `json:"ttlSeconds"`
	ApplyFreshnessRules bool      `json:"applyFreshnessRules"`
	UpdatedAt           time.Time `json:"updatedAt"`
	UpdatedBy           *string   `json:"updatedBy"`
}

// ExtractCacheSaveInput is the caller-supplied mutation spec for
// ExtractCacheStore.Save. Save validates each value into its allowed range
// (TTL: [60, 604800]) and falls back to schema defaults on out-of-range
// rather than rejecting the save.
type ExtractCacheSaveInput struct {
	Enabled             bool
	TTLSeconds          int
	ApplyFreshnessRules bool
	UpdatedBy           string
}

// Bounds mirror the existing prewarm endpoint and Prisma defaults.
const (
	extractCacheMinTTLSeconds     = 60
	extractCacheMaxTTLSeconds     = 7 * 86400 // 7 days
	extractCacheDefaultTTLSeconds = 3600
)

// ExtractCacheStore reads + writes the singleton extract_cache_config row.
type ExtractCacheStore struct {
	pool PgxPool
}

// NewExtractCacheStore returns a store backed by the production pool.
func NewExtractCacheStore(pool *pgxpool.Pool) *ExtractCacheStore {
	return &ExtractCacheStore{pool: pool}
}

// NewExtractCacheStoreWithPgxPool is the test-only constructor.
func NewExtractCacheStoreWithPgxPool(pool PgxPool) *ExtractCacheStore {
	return &ExtractCacheStore{pool: pool}
}

// defaultExtractCacheRow returns the schema-default singleton row used as the
// no-row fallback. Mirrors the Prisma defaults declared on the model.
func defaultExtractCacheRow() *ExtractCacheConfigRow {
	return &ExtractCacheConfigRow{
		ID:                  "singleton",
		Enabled:             true,
		TTLSeconds:          extractCacheDefaultTTLSeconds,
		ApplyFreshnessRules: true,
		UpdatedAt:           time.Time{},
		UpdatedBy:           nil,
	}
}

// Get returns the singleton config row. If no row exists (fresh DB where the
// migration seed was skipped), returns the schema defaults so the caller
// never has to handle a "no row" edge.
func (s *ExtractCacheStore) Get(ctx context.Context) (*ExtractCacheConfigRow, error) {
	const q = `
		SELECT id, enabled, ttl_seconds, apply_freshness_rules,
		       updated_at, updated_by
		FROM extract_cache_config WHERE id = 'singleton'`
	row := &ExtractCacheConfigRow{}
	scanErr := s.pool.QueryRow(ctx, q).Scan(
		&row.ID, &row.Enabled, &row.TTLSeconds, &row.ApplyFreshnessRules,
		&row.UpdatedAt, &row.UpdatedBy,
	)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return defaultExtractCacheRow(), nil
		}
		return nil, fmt.Errorf("configstore: load extract_cache_config: %w", scanErr)
	}
	return row, nil
}

// clampTTL returns ttl when in range, else the schema default. Mirrors
// SemanticCacheStore.Save's permissive normalize-rather-than-reject policy.
func clampExtractTTL(ttl int) int {
	if ttl < extractCacheMinTTLSeconds || ttl > extractCacheMaxTTLSeconds {
		return extractCacheDefaultTTLSeconds
	}
	return ttl
}

// Save upserts the singleton row with the supplied values. Validation falls
// back to schema defaults on out-of-range inputs rather than returning an
// error — the admin UI is expected to validate first, but Save remains safe
// against stale clients.
func (s *ExtractCacheStore) Save(ctx context.Context, in ExtractCacheSaveInput) (*ExtractCacheConfigRow, error) {
	ttl := clampExtractTTL(in.TTLSeconds)
	var updatedBy any
	if in.UpdatedBy != "" {
		updatedBy = in.UpdatedBy
	}
	const q = `
		INSERT INTO extract_cache_config (id, enabled, ttl_seconds, apply_freshness_rules, updated_by, updated_at)
		VALUES ('singleton', $1, $2, $3, $4, NOW())
		ON CONFLICT (id) DO UPDATE SET
			enabled               = EXCLUDED.enabled,
			ttl_seconds           = EXCLUDED.ttl_seconds,
			apply_freshness_rules = EXCLUDED.apply_freshness_rules,
			updated_by            = EXCLUDED.updated_by,
			updated_at            = NOW()
		RETURNING id, enabled, ttl_seconds, apply_freshness_rules, updated_at, updated_by`
	row := &ExtractCacheConfigRow{}
	err := s.pool.QueryRow(ctx, q, in.Enabled, ttl, in.ApplyFreshnessRules, updatedBy).Scan(
		&row.ID, &row.Enabled, &row.TTLSeconds, &row.ApplyFreshnessRules,
		&row.UpdatedAt, &row.UpdatedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("configstore: save extract_cache_config: %w", err)
	}
	return row, nil
}
