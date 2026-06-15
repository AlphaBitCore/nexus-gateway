package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// UsageCache tracks per-period cost usage in Redis (with in-memory fallback).
type UsageCache struct {
	rdb    redis.UniversalClient // nil = in-memory fallback
	logger *slog.Logger

	// In-memory fallback when Redis is unavailable.
	mu       sync.Mutex
	memUsage map[string]int64
}

const usageCachePrefix = "quota:usage:"

// NewUsageCache creates a UsageCache. If rdb is nil, an in-memory map is used.
// Accepts redis.UniversalClient so standalone / sentinel / cluster all work;
// completes the Redis-universal migration, whose earlier pass
// missed this consumer and left cmd/ai-gateway/wiring failing to build.
func NewUsageCache(rdb redis.UniversalClient, logger *slog.Logger) *UsageCache {
	return &UsageCache{
		rdb:      rdb,
		logger:   logger,
		memUsage: make(map[string]int64),
	}
}

// usageKey returns "quota:usage:{targetType}:{targetID}:{periodKey}".
func usageKey(targetType, targetID, periodKey string) string {
	return usageCachePrefix + targetType + ":" + targetID + ":" + periodKey
}

// SetUsageForTest seeds the in-memory usage map with a fixed cost in
// cents for one (target, period) tuple. Intended exclusively for tests
// in sibling packages that need to drive Engine.Check past the
// over-limit threshold without depending on Redis state — production
// code reaches usage through IncrMulti / Backfill. No-op when the cache
// is Redis-backed (rdb != nil).
func (c *UsageCache) SetUsageForTest(targetType, targetID, periodKey string, costCents int64) {
	if c.rdb != nil {
		// Redis-backed caches own their state; the test should write to
		// the backing store directly. Silently no-op here to avoid hiding
		// a race between miniredis and an in-memory copy.
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memUsage[usageKey(targetType, targetID, periodKey)] = costCents
}

// GetUsage returns current cost in cents for a target in a period.
// Returns 0 if not found (cold start case).
func (c *UsageCache) GetUsage(ctx context.Context, targetType, targetID, periodKey string) (int64, error) {
	key := usageKey(targetType, targetID, periodKey)

	if c.rdb != nil {
		val, err := c.rdb.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("usage_cache: GET %s: %w", key, err)
		}
		cents, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("usage_cache: parse %s: %w", val, err)
		}
		return cents, nil
	}

	// In-memory fallback.
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memUsage[key], nil
}

// UsageLevel identifies a quota enforcement target for batch increment.
type UsageLevel struct {
	TargetType string
	TargetID   string
}

// IncrMulti increments usage for multiple levels in one Redis pipeline.
func (c *UsageCache) IncrMulti(ctx context.Context, levels []UsageLevel, periodKey string, costCents int64) error {
	if len(levels) == 0 || costCents <= 0 {
		return nil
	}

	if c.rdb != nil {
		pipe := c.rdb.Pipeline()
		for _, l := range levels {
			key := usageKey(l.TargetType, l.TargetID, periodKey)
			pipe.IncrBy(ctx, key, costCents)
			// EXPIRE overwrites any existing TTL; safe here because periodTTL is
			// absolute to the period end, so re-setting it on every call is
			// idempotent (the TTL always lands on the same wall-clock instant).
			ttl := periodTTL(periodKey)
			if ttl > 0 {
				pipe.Expire(ctx, key, ttl)
			}
		}
		_, err := pipe.Exec(ctx)
		if err != nil {
			return fmt.Errorf("usage_cache: pipeline exec: %w", err)
		}
		return nil
	}

	// In-memory fallback.
	c.mu.Lock()
	for _, l := range levels {
		key := usageKey(l.TargetType, l.TargetID, periodKey)
		c.memUsage[key] += costCents
	}
	c.mu.Unlock()
	return nil
}

// Backfill seeds Redis usage keys from the metrics rollup tables for the
// CURRENT period of every period type actually in use. periodTypes is the set
// returned by PolicyCache.ActivePeriodTypes (e.g. ["daily","monthly"]); a nil
// or empty slice defaults to monthly only. Uses SETNX to avoid overwriting keys
// that already have live-accumulated data. Call once at startup.
//
// Seeding only the monthly key (the previous behaviour) left daily and weekly
// counters at 0 on every restart, so a freshly-booted gateway granted a full
// extra daily/weekly budget until live traffic re-accumulated. Re-seeding each
// active period closes that gap.
func (c *UsageCache) Backfill(ctx context.Context, pool *pgxpool.Pool, periodTypes []string, logger *slog.Logger) error {
	// Typed-nil guard: a nil *pgxpool.Pool stored in the PgxPool interface
	// would compare != nil at the seam, so unwrap to untyped nil here.
	if pool == nil {
		return c.backfillWithPgxPool(ctx, nil, periodTypes, logger)
	}
	return c.backfillWithPgxPool(ctx, pool, periodTypes, logger)
}

// periodWindow returns the period key plus [start, end) window for the current
// period of the given period type, evaluated at now (UTC). The key matches
// CurrentPeriodKey so the backfilled key and the live-traffic key collide and
// SETNX correctly no-ops when live data already exists.
func periodWindow(periodType string, now time.Time) (periodKey string, start, end time.Time) {
	switch periodType {
	case "daily":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
		return now.Format("2006-01-02"), start, end
	case "weekly":
		// Monday 00:00 UTC of the current ISO week through the next Monday.
		// Go weekday Sun=0..Sat=6; ISO Mon=1..Sun=7. Offset from Monday:
		offsetFromMonday := (int(now.Weekday()) + 6) % 7
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		start = dayStart.AddDate(0, 0, -offsetFromMonday)
		end = start.AddDate(0, 0, 7)
		y, w := now.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w), start, end
	default: // monthly
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
		return now.Format("2006-01"), start, end
	}
}

// backfillWithPgxPool is the test-friendly seam — accepts any pgx-compatible
// pool (real *pgxpool.Pool or pgxmock) so unit tests can exercise the rollup
// SQL + pipeline path without a live Postgres.
func (c *UsageCache) backfillWithPgxPool(ctx context.Context, pool PgxPool, periodTypes []string, logger *slog.Logger) error {
	if c.rdb == nil || pool == nil {
		return nil
	}

	// Default to monthly when no policy period types are supplied — preserves
	// the original behaviour for a quota-less or override-less deployment.
	if len(periodTypes) == 0 {
		periodTypes = []string{"monthly"}
	}
	// Dedupe so a config with many daily policies issues one daily backfill.
	seen := make(map[string]struct{}, len(periodTypes))
	uniqueTypes := make([]string, 0, len(periodTypes))
	for _, pt := range periodTypes {
		if _, dup := seen[pt]; dup {
			continue
		}
		seen[pt] = struct{}{}
		uniqueTypes = append(uniqueTypes, pt)
	}

	now := time.Now().UTC()

	// All four enforcement-chain dimensions. The enforcement chain adds a
	// project level (chain.go) and reconcile increments the live project
	// counter, so the boot seed must cover project too — otherwise a Redis
	// cold-start resets the project counter to 0 and a full extra budget of
	// project overspend is allowed until live traffic re-accumulates. The
	// seed query (dim+"=%") and key derivation (usageKey(dim, ...)) are
	// dimension-agnostic, so project flows through the same path.
	dimensions := []string{"user", "virtual_key", "project", "organization"}
	var totalKeys int

	for _, periodType := range uniqueTypes {
		periodKey, periodStart, periodEnd := periodWindow(periodType, now)

		for _, dim := range dimensions {
			rows, err := pool.Query(ctx, `
				SELECT "dimensionKey", SUM(value) AS total_cost
				FROM "metric_rollup_1h"
				WHERE "bucketStart" >= $1 AND "bucketStart" < $2
				  AND "metricName" = 'billed_cost_usd'
				  AND "dimensionKey" LIKE $3
				GROUP BY "dimensionKey"
			`, periodStart, periodEnd, dim+"=%")
			// Uses billed_cost_usd (success only, excludes cache hits) rather than
			// estimated_cost_usd (gross) to avoid cold-start over-counting.
			if err != nil {
				logger.Warn("usage backfill: query failed", "dimension", dim, "periodType", periodType, "error", err)
				continue
			}

			pipe := c.rdb.Pipeline()
			count := 0

			for rows.Next() {
				var dimKey string
				var costUsd float64
				if err := rows.Scan(&dimKey, &costUsd); err != nil {
					continue
				}
				// Extract entityID from "dimension=entityID"
				parts := strings.SplitN(dimKey, "=", 2)
				if len(parts) != 2 || parts[1] == "" {
					continue
				}
				entityID := parts[1]
				costCents := int64(math.Round(costUsd * 100))
				if costCents <= 0 {
					continue
				}

				key := usageKey(dim, entityID, periodKey)
				pipe.SetNX(ctx, key, costCents, periodTTL(periodKey))
				count++
			}
			rows.Close()

			if count > 0 {
				if _, err := pipe.Exec(ctx); err != nil {
					logger.Warn("usage backfill: pipeline exec failed", "dimension", dim, "periodType", periodType, "error", err)
				} else {
					totalKeys += count
				}
			}
		}
	}

	if totalKeys > 0 {
		logger.Info("usage cache backfill completed", "keys", totalKeys, "periodTypes", uniqueTypes)
	}
	return nil
}

// periodTTL returns time until the end of the current period plus a buffer.
func periodTTL(periodKey string) time.Duration {
	now := time.Now().UTC()

	// Try daily: "2006-01-02"
	if t, err := time.Parse("2006-01-02", periodKey); err == nil {
		end := t.AddDate(0, 0, 1).Add(time.Hour) // next day + 1h buffer
		if d := end.Sub(now); d > 0 {
			return d
		}
		return 2 * time.Hour // fallback
	}

	// Try weekly: "2006-W02"
	if len(periodKey) >= 7 && periodKey[4] == '-' && periodKey[5] == 'W' {
		var year, week int
		if _, err := fmt.Sscanf(periodKey, "%d-W%d", &year, &week); err == nil {
			// Find Monday of the given ISO week. Jan 4 is always in ISO week 1.
			// Go's time.Weekday has Sun=0..Sat=6; ISO weekday is Mon=1..Sun=7.
			// Convert: isoDOW = ((Go weekday + 6) % 7) gives Mon=0..Sun=6 offset
			// from Monday, so subtracting it from Jan 4 lands on the Monday of
			// week 1. Note: (Go weekday - Monday) is -1 for years where Jan 4
			// falls on a Sunday and would produce a Monday 7 days too late.
			jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
			mondayOffsetFromJan4 := (int(jan4.Weekday()) + 6) % 7
			week1Monday := jan4.AddDate(0, 0, -mondayOffsetFromJan4)
			monday := week1Monday.AddDate(0, 0, (week-1)*7)
			nextMonday := monday.AddDate(0, 0, 7).Add(time.Hour)
			if d := nextMonday.Sub(now); d > 0 {
				return d
			}
			return 8 * 24 * time.Hour // fallback
		}
	}

	// Try monthly: "2006-01"
	if t, err := time.Parse("2006-01", periodKey); err == nil {
		end := t.AddDate(0, 1, 0).Add(time.Hour) // next month + 1h buffer
		if d := end.Sub(now); d > 0 {
			return d
		}
		return 32 * 24 * time.Hour // fallback
	}

	// Unknown format — default 32 days.
	return 32 * 24 * time.Hour
}
