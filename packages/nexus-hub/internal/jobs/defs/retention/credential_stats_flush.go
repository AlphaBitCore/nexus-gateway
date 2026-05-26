package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

const (
	credStatsFlushJobID   = "credential-stats-flush"
	credStatsFlushJobName = "Credential Stats Flush"
	credStatsFlushJobDesc = "Drains per-credential usage counters and timestamps from Redis into the Credential table. Runs frequently to keep lastUsedAt, lastSuccessAt, lastFailureAt, and totalUsageCount up to date without high-frequency concurrent DB writes."
)

// luaReadAndResetCount atomically reads the cnt field and resets it to 0.
// The literal 'cnt' must stay in lockstep with credstate.StatsFieldCount.
var luaReadAndResetCount = redis.NewScript(`
local cnt = tonumber(redis.call('HGET', KEYS[1], 'cnt') or '0')
if cnt and cnt > 0 then
  redis.call('HSET', KEYS[1], 'cnt', 0)
end
return cnt or 0
`)

// CredentialStatsFlushJob drains per-credential Redis usage stats into the DB.
// AI Gateway writes delta counters and timestamps to Redis on every upstream
// attempt; this job batches them into the Credential table to avoid concurrent
// write contention.
type CredentialStatsFlushJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the flush
	// UPDATE can be unit-tested via pgxmock without writing real
	// Credential rows.
	pool     defs.PgxPool
	rdb      redis.UniversalClient
	interval time.Duration
	logger   *slog.Logger
}

// NewCredentialStatsFlush constructs the job. interval defaults to 60s.
func NewCredentialStatsFlush(pool *pgxpool.Pool, rdb redis.UniversalClient, interval time.Duration, logger *slog.Logger) *CredentialStatsFlushJob {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &CredentialStatsFlushJob{
		pool:     pool,
		rdb:      rdb,
		interval: interval,
		logger:   logger.With("job", credStatsFlushJobID),
	}
}

func (j *CredentialStatsFlushJob) ID() string              { return credStatsFlushJobID }
func (j *CredentialStatsFlushJob) Name() string            { return credStatsFlushJobName }
func (j *CredentialStatsFlushJob) Description() string     { return credStatsFlushJobDesc }
func (j *CredentialStatsFlushJob) Interval() time.Duration { return j.interval }

func (j *CredentialStatsFlushJob) Run(ctx context.Context) error {
	if j.rdb == nil {
		return nil
	}

	// Atomically claim all dirty credential IDs and remove from the set.
	// Any new dirty entries added after SMEMBERS but before SREM will be
	// re-added by the next AI Gateway write and picked up on the next run.
	ids, err := j.rdb.SMembers(ctx, credstate.StatsDirtySet).Result()
	if errors.Is(err, redis.Nil) || len(ids) == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("credential-stats-flush: read dirty set: %w", err)
	}

	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if e := j.rdb.SRem(ctx, credstate.StatsDirtySet, args...).Err(); e != nil {
		j.logger.Warn("credential-stats-flush: failed to remove dirty IDs from set, will retry next cycle", "error", e)
	}

	var flushed, skipped int
	for _, credID := range ids {
		if err := j.flushOne(ctx, credID); err != nil {
			j.logger.Warn("credential-stats-flush: flush failed", "credentialID", credID, "error", err)
			skipped++
		} else {
			flushed++
		}
	}
	j.logger.Debug("credential-stats-flush: done", "flushed", flushed, "skipped", skipped)
	return nil
}

func (j *CredentialStatsFlushJob) flushOne(ctx context.Context, credID string) error {
	key := credstate.StatsKey(credID)

	// Atomically read and reset the usage delta counter.
	delta, err := luaReadAndResetCount.Run(ctx, j.rdb, []string{key}).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("read delta: %w", err)
	}

	// Read timestamp fields (best-effort; last-write-wins is acceptable).
	vals, err := j.rdb.HMGet(ctx, key,
		credstate.StatsFieldUsedAt, credstate.StatsFieldOkAt, credstate.StatsFieldFailAt, credstate.StatsFieldFailReason,
	).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("read timestamps: %w", err)
	}

	str := func(i int) *string {
		if i >= len(vals) || vals[i] == nil {
			return nil
		}
		s, ok := vals[i].(string)
		if !ok || s == "" {
			return nil
		}
		return &s
	}
	parseTS := func(s *string) *time.Time {
		if s == nil {
			return nil
		}
		t, err := time.Parse(time.RFC3339Nano, *s)
		if err != nil {
			return nil
		}
		return &t
	}

	usedAt := parseTS(str(0))
	okAt := parseTS(str(1))
	failAt := parseTS(str(2))
	failReason := str(3)

	if delta == 0 && usedAt == nil && okAt == nil && failAt == nil {
		return nil // nothing to write
	}

	_, err = j.pool.Exec(ctx, `
		UPDATE "Credential" SET
			"totalUsageCount" = "totalUsageCount" + $2,
			"lastUsedAt"      = CASE WHEN $3::timestamptz IS NOT NULL AND ($3::timestamptz > "lastUsedAt" OR "lastUsedAt" IS NULL) THEN $3::timestamptz ELSE "lastUsedAt" END,
			"lastSuccessAt"   = CASE WHEN $4::timestamptz IS NOT NULL AND ($4::timestamptz > "lastSuccessAt" OR "lastSuccessAt" IS NULL) THEN $4::timestamptz ELSE "lastSuccessAt" END,
			"lastFailureAt"   = CASE WHEN $5::timestamptz IS NOT NULL AND ($5::timestamptz > "lastFailureAt" OR "lastFailureAt" IS NULL) THEN $5::timestamptz ELSE "lastFailureAt" END,
			"lastFailureReason" = CASE WHEN $5::timestamptz IS NOT NULL AND ($5::timestamptz > "lastFailureAt" OR "lastFailureAt" IS NULL) THEN $6 ELSE "lastFailureReason" END,
			"updatedAt"       = NOW()
		WHERE id = $1
	`, credID, delta, usedAt, okAt, failAt, failReason)
	if err != nil {
		return fmt.Errorf("db update: %w", err)
	}
	return nil
}
