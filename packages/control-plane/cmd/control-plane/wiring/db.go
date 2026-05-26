// Package wiring assembles the Control Plane's subsystems from config.
// Each Init* function initialises one subsystem and returns the live
// handle(s) the caller wires into downstream Init* calls or the HTTP server.
package wiring

import (
	"context"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// InitDB opens a pgxpool connection to Postgres and returns the DB handle plus
// a closer. Returns (nil, nil-closer, nil) when cfg.Database.URL is empty so
// the caller can skip DB-dependent subsystems gracefully.
// The caller must call the closer on shutdown even when db is nil.
func InitDB(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*store.DB, func(), error) {
	if cfg.Database.URL == "" {
		logger.Warn("DATABASE_URL not set — running without database")
		return nil, func() {}, nil
	}
	db, err := store.New(ctx, cfg.Database.URL, store.PoolConfig{
		MaxConns:        cfg.Database.MaxConns,
		MinConns:        cfg.Database.MinConns,
		MaxConnLifetime: cfg.Database.MaxConnLifetime,
	})
	if err != nil {
		return nil, func() {}, err
	}
	logger.Info("database connected")
	return db, db.Close, nil
}
