package health

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// testLogger returns a discard logger for use in unit tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// healthRollupTestPool opens a pgxpool against TEST_DATABASE_URL, skipping
// the test if the env var is unset or the database is unreachable.
func healthRollupTestPool(t *testing.T) *pgxpool.Pool {
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

// seedCredential inserts a minimal Credential row with the supplied
// circuit fields. Returns the credential ID + cleanup callback.
func seedCredential(t *testing.T, pool *pgxpool.Pool, providerID string, state, reason string, openedAt, nextProbeAt *time.Time) (string, func()) {
	t.Helper()
	id := uuid.NewString()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO "Credential" (
			id, name, "providerId",
			"encryptedKey", "encryptionIv", "encryptionTag",
			enabled,
			"circuitState", "circuitReason", "circuitOpenedAt", "circuitNextProbeAt",
			"createdAt", "updatedAt"
		)
		VALUES (
			$1, $1, $2,
			'ct', 'iv', 'tag',
			TRUE,
			$3, $4, $5, $6,
			NOW(), NOW()
		)
	`, id, providerID, state, reason, openedAt, nextProbeAt)
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return id, func() { _, _ = pool.Exec(ctx, `DELETE FROM "Credential" WHERE id = $1`, id) }
}

// ensureTestProvider returns an existing or freshly-inserted Provider ID.
func ensureTestProvider(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `SELECT id FROM "Provider" LIMIT 1`).Scan(&id)
	if err == nil {
		return id
	}
	id = uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO "Provider" (id, name, "adapterType", "baseUrl", "pathPrefix", enabled, "createdAt", "updatedAt")
		VALUES ($1, $1, 'openai', 'https://example.invalid', '/v1', TRUE, NOW(), NOW())
	`, id); err != nil {
		t.Fatalf("seed provider fallback: %v", err)
	}
	return id
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

func (f *fakeRaiser) raiseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raises)
}

func (f *fakeRaiser) resolveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resolves)
}

// errRaiser is a fakeRaiser variant that returns configured errors.
type errRaiser struct {
	raiseErr   error
	resolveErr error
}

func (e *errRaiser) Raise(_ context.Context, _ alerting.RaiseInput) error { return e.raiseErr }
func (e *errRaiser) Resolve(_ context.Context, _, _, _ string) error      { return e.resolveErr }

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

// jobsTestCleanup removes test alert rows and closes the pool.
func jobsTestCleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertDispatch" WHERE "alertId" IN (SELECT id FROM "Alert" WHERE "ruleId" LIKE 'test.job.%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" LIKE 'test.job.%' OR "targetKey" LIKE 'override:test-job-%' OR "targetKey" LIKE 'policy:test-job-%' OR "targetKey" LIKE 'vk:test-job-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertRule" WHERE id LIKE 'test.job.%'`)
	pool.Close()
}
