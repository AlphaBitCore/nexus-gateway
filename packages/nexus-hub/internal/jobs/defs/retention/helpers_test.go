package retention

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testLogger returns a discard logger for use in unit tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureLogger writes into w so a test can assert on what an operator would
// actually see. Some of these jobs report a degradation ONLY through a log
// line — a skipped row, an unreadable one — and a discard logger makes that
// class of behaviour unassertable.
func captureLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// jobsTestPool opens a pgxpool against TEST_DATABASE_URL, skipping the test
// if the env var is unset or the database is unreachable.
func jobsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-backed integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}
