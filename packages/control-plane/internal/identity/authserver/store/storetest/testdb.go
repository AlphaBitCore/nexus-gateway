// Package storetest exposes a pgxpool helper for authserver store integration
// tests. Tests skip automatically when DATABASE_URL is unset so the suite stays
// green without a live Postgres.
package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns a pgxpool connected to DATABASE_URL. If the env var is unset,
// the test is skipped. The pool is closed automatically via t.Cleanup.
func Open(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
