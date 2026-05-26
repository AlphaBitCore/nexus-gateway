package integrationgo_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/jackc/pgx/v5/pgxpool"
)

// setupOrSkip resolves Env, opens the pgx pool, and skips the calling
// test if NEXUS_TEST_VK is unset or the placeholder. Skip (rather than
// fail) keeps the suite green on a fresh checkout where the operator
// hasn't filled in tests/.env.test yet — same contract the Python tests
// use.
func setupOrSkip(t *testing.T) (*helpers.Env, string, *pgxpool.Pool) {
	t.Helper()
	env, err := helpers.LoadEnv()
	if err != nil {
		t.Fatalf("load env: %v", err)
	}
	if env.TestVK == "" || env.TestVK == "nvk_REPLACE_ME" {
		t.Skip("NEXUS_TEST_VK not set in tests/.env.test — skipping integration test")
	}
	db, err := helpers.DB(context.Background(), env)
	if err != nil {
		t.Fatalf("open DB pool: %v", err)
	}
	return env, env.TestVK, db
}

// mustMarshal serialises v to JSON or fails the test. Exists so test
// bodies stay readable: `body := mustMarshal(t, map[string]any{...})`.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return b
}

// truncate is a tiny helper for HTTP error logging — full bodies blow up
// test logs without adding signal.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
