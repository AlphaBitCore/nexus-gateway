package ws

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const testServiceToken = "test-service-token"

// newTestServer creates a minimal Server wired to the live DB from DATABASE_URL.
// Redis and MQ are nil (nil-guarded throughout the codebase).
// The test is skipped when DATABASE_URL is not set so non-DB CI stays green.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("set DATABASE_URL to run DB-backed ws tests")
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("database ping: %v", err)
	}
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Use a per-test unique opsmetrics registry (backed by a private
	// prometheus.Registry) to avoid duplicate-registration panics when
	// multiple tests in this package each call newTestServer.
	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())

	st := store.New(pool)
	wsPool := NewPool(opsReg, logger)
	mgr := manager.New(st, nil, nil, wsPool, "test-hub", logger)

	return NewServer(wsPool, mgr, "test-hub", testServiceToken, nil, true, logger)
}

// TestAuthenticate_RejectsTokenQueryParam verifies that the ?token= query-
// parameter fallback is no longer accepted — even with a valid service token.
// Tokens must arrive via the Authorization header or the Sec-WebSocket-Protocol
// subprotocol. Without this guard a valid token leaks into proxy access logs.
func TestAuthenticate_RejectsTokenQueryParam(t *testing.T) {
	s := newTestServer(t)
	// Use the real service token in the query param: pre-fix code would accept
	// this (extractBearerToken returned it), post-fix code must reject it.
	req := httptest.NewRequest(http.MethodGet, "/ws?id=x&type=agent&token="+testServiceToken, nil)
	_, _, err := s.authenticate(req)
	if err == nil {
		t.Fatal("expected authentication to fail for ?token= fallback; got success")
	}
}

// TestAuthenticate_RejectsPlaintextMetadataToken verifies that a Thing whose
// metadata contains a legacy plaintext "token" field cannot authenticate via
// that field. Path 3 (the C5 vulnerability) must not exist in the codebase.
func TestAuthenticate_RejectsPlaintextMetadataToken(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)

	thingID := fmt.Sprintf("agent-legacy-%d", time.Now().UnixNano())

	// Pre-delete any stale row from a previous interrupted run.
	// newTestServer wires a concrete *manager.Manager; type-assert to
	// reach its Store() for the seed/teardown SQL.
	mgr := s.mgr.(*manager.Manager)
	pool := mgr.Store().Pool()
	_, _ = pool.Exec(ctx, "DELETE FROM thing WHERE id = $1", thingID)

	// Seed a Thing whose metadata contains a raw token (simulating legacy state).
	err := mgr.Store().RegistryStore().UpsertThingEnrollment(ctx, store.UpsertThingParams{
		ID:       thingID,
		Type:     "agent",
		Metadata: map[string]any{"token": "legacy-secret"},
		Status:   "online",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Cleanup(func() {
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM thing WHERE id = $1`, thingID)
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ws?id=%s", thingID), nil)
	req.Header.Set("Authorization", "Bearer legacy-secret")

	_, _, authErr := s.authenticate(req)
	if authErr == nil {
		t.Fatal("expected authentication to fail for plaintext metadata.token; got success")
	}
}
