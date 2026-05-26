// DB-integration tests for CredentialCircuitFlushJob.
// Real Postgres (skips when unreachable) + real miniredis. No mocks.

package retention

import (
	"context"
	"encoding/hex"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// circuitFlushTestPool returns a pgx pool against the dev DB. Skips the
// suite when DB is unreachable so unit tests still pass on machines
// without a running stack.
//
// Foreign-data safety: every Credential row this test creates uses a
// fresh uuid (see seedCredential) and is removed by the per-test cleanup
// closure. The job's Run() only UPDATEs credentials whose id was placed
// in cred:circuit:dirty by this test — the Redis hash is backed by a
// per-test miniredis so there is no cross-test contamination. The
// first-run rehydrate may READ other Credential rows but only WRITEs to
// the per-test miniredis, never to foreign DB rows.
func circuitFlushTestPool(t *testing.T) *pgxpool.Pool {
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
// circuit fields. Returns the credential ID + cleanup callback. All
// IDs share a prefix so concurrent test runs don't collide.
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

// ensureTestProvider returns an existing or freshly-inserted Provider ID
// the credential rows can FK onto. We pick the first row in the table —
// seeded by the dev seed — and fall back to creating one if the DB is
// empty (so the test still runs against a brand-new local).
func ensureTestProvider(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `SELECT id FROM "Provider" LIMIT 1`).Scan(&id)
	if err == nil {
		return id
	}
	// Empty provider table — seed one.
	id = uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO "Provider" (id, name, "adapterType", "baseUrl", "pathPrefix", enabled, "createdAt", "updatedAt")
		VALUES ($1, $1, 'openai', 'https://example.invalid', '/v1', TRUE, NOW(), NOW())
	`, id); err != nil {
		t.Fatalf("seed provider fallback: %v", err)
	}
	return id
}

// newCircuitFlushTestEnv builds a fully wired flush job (real DB + miniredis).
func newCircuitFlushTestEnv(t *testing.T) (*CredentialCircuitFlushJob, *pgxpool.Pool, *redis.Client, string) {
	t.Helper()
	pool := circuitFlushTestPool(t)
	t.Cleanup(pool.Close)

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mini.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})

	hubID := "hub-test-" + randHex(t, 4)
	job := NewCredentialCircuitFlush(pool, rdb, hubID, 30*time.Second, testLogger(), nil)
	return job, pool, rdb, hubID
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.New(rand.NewSource(time.Now().UnixNano())).Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// TestCircuitFlush_PersistsRateLimitOpen seeds a fresh credential, marks
// its Redis hash as a rate_limit OPEN circuit, then asserts Run() persists
// every column.
func TestCircuitFlush_PersistsRateLimitOpen(t *testing.T) {
	job, pool, rdb, _ := newCircuitFlushTestEnv(t)
	providerID := ensureTestProvider(t, pool)
	id, cleanup := seedCredential(t, pool, providerID, credstate.CircuitClosed, "", nil, nil)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	probeAt := now.Add(60 * time.Second)
	if err := rdb.HSet(ctx, credstate.CircuitKey(id),
		credstate.CircuitFieldState, credstate.CircuitOpen,
		credstate.CircuitFieldOpenReason, credstate.ReasonRateLimit,
		credstate.CircuitFieldOpenedAt, now.Format(time.RFC3339Nano),
		credstate.CircuitFieldNextProbe, probeAt.Format(time.RFC3339Nano),
	).Err(); err != nil {
		t.Fatalf("seed redis hash: %v", err)
	}
	if err := rdb.SAdd(ctx, credstate.CircuitDirtySet, id).Err(); err != nil {
		t.Fatalf("seed dirty: %v", err)
	}

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var state, reason string
	var openedAt, next *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT "circuitState", "circuitReason", "circuitOpenedAt", "circuitNextProbeAt" FROM "Credential" WHERE id=$1`,
		id,
	).Scan(&state, &reason, &openedAt, &next); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != credstate.CircuitOpen || reason != credstate.ReasonRateLimit {
		t.Fatalf("circuit not persisted: state=%q reason=%q", state, reason)
	}
	if openedAt == nil || next == nil {
		t.Fatalf("timestamps not persisted: openedAt=%v nextProbeAt=%v", openedAt, next)
	}
	// In-flight set should be empty after a clean cycle.
	members, _ := rdb.SMembers(ctx, "cred:circuit:in_flight:"+job.hubID).Result()
	if len(members) != 0 {
		t.Fatalf("in-flight not drained: %v", members)
	}
}

// TestCircuitFlush_EmptyHashResetsToClosed simulates a recovery DEL —
// AI Gateway closed the circuit, dirty set has the credID, but the hash
// is gone. Job must explicitly reset the DB columns to closed.
func TestCircuitFlush_EmptyHashResetsToClosed(t *testing.T) {
	job, pool, rdb, _ := newCircuitFlushTestEnv(t)
	providerID := ensureTestProvider(t, pool)
	now := time.Now().UTC()
	// Seed DB with an OPEN row.
	id, cleanup := seedCredential(t, pool, providerID, credstate.CircuitOpen, credstate.ReasonAuthFail, &now, nil)
	defer cleanup()

	ctx := context.Background()
	if err := rdb.SAdd(ctx, credstate.CircuitDirtySet, id).Err(); err != nil {
		t.Fatalf("seed dirty: %v", err)
	}
	// Note: NO HSET on cred:circuit:{id} — empty hash = closed.

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var state string
	var reason *string
	var openedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT "circuitState", "circuitReason", "circuitOpenedAt" FROM "Credential" WHERE id=$1`,
		id,
	).Scan(&state, &reason, &openedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != credstate.CircuitClosed || reason != nil || openedAt != nil {
		t.Fatalf("reset to closed failed: state=%q reason=%v openedAt=%v", state, reason, openedAt)
	}
}

// TestCircuitFlush_FirstRunRehydratesFromDB verifies the post-restart
// recovery path: DB has a non-closed circuit, Redis is empty → hash
// should be restored.
func TestCircuitFlush_FirstRunRehydratesFromDB(t *testing.T) {
	job, pool, rdb, _ := newCircuitFlushTestEnv(t)
	providerID := ensureTestProvider(t, pool)
	openedAt := time.Now().UTC().Add(-2 * time.Minute)
	id, cleanup := seedCredential(t, pool, providerID,
		credstate.CircuitOpen, credstate.ReasonAuthFail, &openedAt, nil)
	defer cleanup()

	ctx := context.Background()
	// Run with NOTHING in Redis. First-run rehydrate should restore the hash.
	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	state, err := rdb.HGet(ctx, credstate.CircuitKey(id), credstate.CircuitFieldState).Result()
	if err != nil {
		t.Fatalf("HGET state: %v", err)
	}
	if state != credstate.CircuitOpen {
		t.Fatalf("rehydrate state: got %q want open", state)
	}
	reason, _ := rdb.HGet(ctx, credstate.CircuitKey(id), credstate.CircuitFieldOpenReason).Result()
	if reason != credstate.ReasonAuthFail {
		t.Fatalf("rehydrate reason: got %q want auth_fail", reason)
	}
}

// TestCircuitFlush_RehydrateSkipsExpiredRateLimit: a rate_limit circuit
// whose cooldown is already in the past should NOT be rehydrated —
// re-opening would be a stale signal.
func TestCircuitFlush_RehydrateSkipsExpiredRateLimit(t *testing.T) {
	job, pool, rdb, _ := newCircuitFlushTestEnv(t)
	providerID := ensureTestProvider(t, pool)
	past := time.Now().UTC().Add(-2 * time.Minute)
	id, cleanup := seedCredential(t, pool, providerID,
		credstate.CircuitOpen, credstate.ReasonRateLimit, &past, &past)
	defer cleanup()

	ctx := context.Background()
	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	n, err := rdb.Exists(ctx, credstate.CircuitKey(id)).Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no rehydrate for expired rate-limit, got Exists=%d", n)
	}
}

// TestCircuitFlush_ReclaimsAfterCrash: a prior in-flight set is moved
// back to dirty before the new cohort is processed (at-least-once).
func TestCircuitFlush_ReclaimsAfterCrash(t *testing.T) {
	job, pool, rdb, _ := newCircuitFlushTestEnv(t)
	providerID := ensureTestProvider(t, pool)
	id, cleanup := seedCredential(t, pool, providerID, credstate.CircuitClosed, "", nil, nil)
	defer cleanup()

	ctx := context.Background()
	// Simulate a crashed prior run that left id in the in-flight set,
	// with a fresh open state in the Redis hash.
	if err := rdb.HSet(ctx, credstate.CircuitKey(id),
		credstate.CircuitFieldState, credstate.CircuitOpen,
		credstate.CircuitFieldOpenReason, credstate.ReasonAuthFail,
		credstate.CircuitFieldOpenedAt, time.Now().UTC().Format(time.RFC3339Nano),
	).Err(); err != nil {
		t.Fatalf("seed hash: %v", err)
	}
	inFlight := credstate.InFlightSet(job.hubID)
	if err := rdb.SAdd(ctx, inFlight, id).Err(); err != nil {
		t.Fatalf("seed in-flight: %v", err)
	}

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// After reclaim → flush, the DB should reflect the OPEN state.
	var state string
	if err := pool.QueryRow(ctx,
		`SELECT "circuitState" FROM "Credential" WHERE id=$1`, id,
	).Scan(&state); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != credstate.CircuitOpen {
		t.Fatalf("reclaim → flush failed: state=%q", state)
	}
}

// testLogger() is defined in stale_thing_test.go — package-scoped helper
// reused across all DB-integration tests.
