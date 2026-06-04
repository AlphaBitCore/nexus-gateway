// Package wiring wires all nexus-hub subsystems together. Each file in this
// package corresponds to one subsystem. main.go calls Init* functions in
// dependency order.
package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
)

// InitDB creates and pings the pgxpool, then starts the background pool
// watcher. Returns the pool or an error; the caller is responsible for
// calling pool.Close() on shutdown.
func InitDB(ctx context.Context, cfg *config.HubConfig, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, err
	}
	poolCfg.MaxConns = cfg.Database.MaxConns
	poolCfg.MinConns = cfg.Database.MinConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	logger.Info("database connected")

	go watchPgxpool(ctx, pool, logger)
	return pool, nil
}

// pgxpoolStats is the flattened stat surface watchPgxpool reads per tick.
// Returned by pgxpoolStatter.Stat() via the pgxpoolStatSnapshot adapter so
// that tests can provide their own values without constructing a
// pgxpool.Stat (whose internal puddle.Stat pointer is unexported and would
// panic on a zero-value struct).
type pgxpoolStats struct {
	MaxConns             int32
	AcquiredConns        int32
	IdleConns            int32
	TotalConns           int32
	NewConnsCount        int64
	AcquireCount         int64
	EmptyAcquireCount    int64
	CanceledAcquireCount int64
}

// pgxpoolStatter is the narrow interface watchPgxpool needs from *pgxpool.Pool.
// Tests may inject a fake implementation via the watchPgxpoolWithStatter seam.
type pgxpoolStatter interface {
	PoolStats() pgxpoolStats
}

// pgxpoolStatSnapshot wraps *pgxpool.Pool and satisfies pgxpoolStatter.
// Production code always calls watchPgxpool which wraps the real pool here.
type pgxpoolStatSnapshot struct{ pool *pgxpool.Pool }

func (w pgxpoolStatSnapshot) PoolStats() pgxpoolStats {
	s := w.pool.Stat()
	return pgxpoolStats{
		MaxConns:             s.MaxConns(),
		AcquiredConns:        s.AcquiredConns(),
		IdleConns:            s.IdleConns(),
		TotalConns:           s.TotalConns(),
		NewConnsCount:        s.NewConnsCount(),
		AcquireCount:         s.AcquireCount(),
		EmptyAcquireCount:    s.EmptyAcquireCount(),
		CanceledAcquireCount: s.CanceledAcquireCount(),
	}
}

// watchPgxpool periodically samples the pgxpool stats and surfaces them in
// logs so a stuck Hub can be diagnosed without a debugger. Emits an INFO line
// every minute and an additional WARN line whenever utilization
// (acquired_conns / max_conns) crosses 70% — the classic shape of a leaked
// connection or a tx held across a goroutine boundary. Exits when ctx is done.
func watchPgxpool(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	watchPgxpoolWithStatter(ctx, pgxpoolStatSnapshot{pool: pool}, logger)
}

// watchPgxpoolSampleInterval controls the ticker cadence for pool sampling.
// Declared as a var (not const) so tests can lower the interval without
// affecting production behaviour.
var watchPgxpoolSampleInterval = 30 * time.Second

func watchPgxpoolWithStatter(ctx context.Context, pool pgxpoolStatter, logger *slog.Logger) {
	const (
		infoInterval  = 1 * time.Minute
		highThreshold = 0.70
	)
	t := time.NewTicker(watchPgxpoolSampleInterval)
	defer t.Stop()
	var nextInfo time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s := pool.PoolStats()
			max := s.MaxConns
			acq := s.AcquiredConns
			args := []any{
				"acquired_conns", acq,
				"idle_conns", s.IdleConns,
				"total_conns", s.TotalConns,
				"max_conns", max,
				"new_conns_count", s.NewConnsCount,
				"acquire_count", s.AcquireCount,
				"empty_acquire_count", s.EmptyAcquireCount,
				"canceled_acquire_count", s.CanceledAcquireCount,
			}
			if max > 0 && float64(acq)/float64(max) >= highThreshold {
				logger.Warn("db pool high utilization", args...)
				continue
			}
			if now.After(nextInfo) {
				logger.Info("db pool stats", args...)
				nextInfo = now.Add(infoInterval)
			}
		}
	}
}
