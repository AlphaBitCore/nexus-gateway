// Package wiring assembles ai-gateway subsystems from config and shared
// components. Each file owns one subsystem; Init* functions are exported so
// the cmd/ai-gateway/main.go orchestrator can call them in sequence.
package wiring

import (
	"context"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// InitDB opens a pgxpool connection and returns a *store.DB.
// Returns (nil, nil) when DATABASE_URL is absent — callers must handle
// the nil case gracefully (degraded mode without database).
func InitDB(ctx context.Context, dsn string) (*store.DB, error) {
	if dsn == "" {
		slog.Warn("DATABASE_URL not set, running without database")
		return nil, nil
	}
	db, err := store.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	slog.Info("database connected")
	return db, nil
}
