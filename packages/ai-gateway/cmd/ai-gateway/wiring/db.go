// Package wiring assembles ai-gateway subsystems from config and shared
// components. Each file owns one subsystem; Init* functions are exported so
// the cmd/ai-gateway/main.go orchestrator can call them in sequence.
package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// InitDB opens a pgxpool connection and returns a *store.DB, applying the
// configured pool tuning (MaxConns/MinConns/MaxConnLifetime) so the hot
// request path is not capped at the pgx default of max(4, NumCPU).
// Returns (nil, nil) when DATABASE_URL is absent — callers must handle
// the nil case gracefully (degraded mode without database).
func InitDB(ctx context.Context, dbCfg config.DatabaseConfig) (*store.DB, error) {
	if dbCfg.URL == "" {
		slog.Warn("DATABASE_URL not set, running without database")
		return nil, nil
	}
	db, err := store.New(ctx, dbCfg.URL, store.PoolConfig{
		MaxConns:        dbCfg.MaxConns,
		MinConns:        dbCfg.MinConns,
		MaxConnLifetime: time.Duration(dbCfg.MaxConnLifetimeSec) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("database connected")
	return db, nil
}
