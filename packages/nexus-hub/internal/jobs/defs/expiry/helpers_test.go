package expiry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// testLogger returns a discard logger for use in unit tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

// fakeRaiser records Raise/Resolve calls without touching a database.
type fakeRaiser struct {
	mu       sync.Mutex
	raises   []alerting.RaiseInput
	resolves []resolveCall
}

type resolveCall struct {
	RuleID    string
	TargetKey string
	Reason    string
}

func (f *fakeRaiser) Raise(_ context.Context, in alerting.RaiseInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raises = append(f.raises, in)
	return nil
}

func (f *fakeRaiser) Resolve(_ context.Context, ruleID, targetKey, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, resolveCall{RuleID: ruleID, TargetKey: targetKey, Reason: reason})
	return nil
}

// jobsTestCleanup removes test alert rows and closes the pool.
func jobsTestCleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertDispatch" WHERE "alertId" IN (SELECT id FROM "Alert" WHERE "ruleId" LIKE 'test.job.%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" LIKE 'test.job.%' OR "targetKey" LIKE 'override:test-job-%' OR "targetKey" LIKE 'policy:test-job-%' OR "targetKey" LIKE 'vk:test-job-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertRule" WHERE id LIKE 'test.job.%'`)
	pool.Close()
}
