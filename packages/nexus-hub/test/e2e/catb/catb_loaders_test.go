//go:build e2e

// Package catb holds end-to-end tests for the Cat B loader registry
// introduced in P0-C. It lives in a sub-package (rather than sharing
// test/e2e/) so it can compile under -tags e2e independently of the
// pre-existing break in testharness/harness.go that the sibling e2e
// files inherit.
package catb

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// randRead is a thin wrapper around crypto/rand.Reader so the helper
// is explicit about its source.
func randRead(p []byte) (int, error) { return io.ReadFull(rand.Reader, p) }

// openMainDBPool connects to DATABASE_URL; tests are build-tagged `e2e`
// and skip gracefully when the env is absent so `go test ./...` without
// the tag keeps passing in CI.
func openMainDBPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; skipping e2e")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestCatB_AgentHookConfig_Live seeds one enabled HookConfig row, calls
// the loader, and asserts the marshalled JSON envelope matches what
// AgentPipeline.ApplyHooksShadowState expects. Rolls back the seed in
// t.Cleanup to keep the DB pristine.
func TestCatB_AgentHookConfig_Live(t *testing.T) {
	pool := openMainDBPool(t)
	ctx := context.Background()

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "HookConfig" (id, name, type, "implementationId", stage,
		                         category, endpoint, script, config,
		                         priority, "timeoutMs", "failBehavior", enabled,
		                         "applicableIngress", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), 'catb-e2e-pii', 'builtin', 'pii-detector', 'request',
		        NULL, NULL, NULL, '{"mode":"block"}'::jsonb,
		        10, 5000, 'fail-open', true, ARRAY['ALL']::text[], NOW(), NOW())
		RETURNING id
	`).Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "HookConfig" WHERE id = $1`, id)
	})

	l := store.NewAgentHookConfigLoader(pool, nil, nil)
	state, ver, err := l.Load(ctx, "thing-e2e")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver == 0 {
		t.Error("expected non-zero version from live row")
	}
	raw, _ := json.Marshal(state)
	// We don't byte-match because other rows may exist; assert the
	// envelope shape and that our seeded row is present.
	var env struct {
		HookConfigs []map[string]any `json:"hookConfigs"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, h := range env.HookConfigs {
		if h["id"] == id {
			found = true
			if h["implementationId"] != "pii-detector" {
				t.Errorf("implementationId = %v want pii-detector", h["implementationId"])
			}
			if h["stage"] != "request" {
				t.Errorf("stage = %v want request", h["stage"])
			}
		}
	}
	if !found {
		t.Fatalf("seeded hook not found in %s", raw)
	}
}

// TestCatB_AgentInterceptionDomains_Live seeds a domain with one path
// and asserts the nested envelope shape.
func TestCatB_AgentInterceptionDomains_Live(t *testing.T) {
	pool := openMainDBPool(t)
	ctx := context.Background()

	var domID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "interception_domain" (id, name, host_pattern, host_match_type,
			adapter_id, adapter_config, enabled, priority,
			default_path_action, on_adapter_error, network_zone,
			source, created_at, updated_at)
		VALUES (gen_random_uuid(), 'catb-e2e-dom', 'e2e.example.com', 'EXACT',
		        'openai-compat', NULL, true, 100,
		        'PROCESS', 'FAIL_OPEN', 'PUBLIC',
		        'admin', NOW(), NOW())
		RETURNING id
	`).Scan(&domID); err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "interception_domain" WHERE id = $1`, domID)
	})

	var pathID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "interception_path" (id, domain_id, path_pattern, match_type,
			action, priority, enabled, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, ARRAY['/v1/chat/completions']::text[], 'PREFIX',
		        'PROCESS', 10, true, NOW(), NOW())
		RETURNING id
	`, domID).Scan(&pathID); err != nil {
		t.Fatalf("seed path: %v", err)
	}

	l := store.NewAgentInterceptionDomainsLoader(pool, nil)
	state, ver, err := l.Load(ctx, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver == 0 {
		t.Error("expected non-zero version")
	}
	raw, _ := json.Marshal(state)
	var env struct {
		InterceptionDomains []struct {
			ID    string `json:"id"`
			Paths []struct {
				ID string `json:"id"`
			} `json:"paths"`
		} `json:"interceptionDomains"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, d := range env.InterceptionDomains {
		if d.ID == domID {
			found = true
			if len(d.Paths) != 1 || d.Paths[0].ID != pathID {
				t.Errorf("seeded path missing or mis-attributed: %+v", d.Paths)
			}
		}
	}
	if !found {
		t.Fatalf("seeded domain not found in %s", raw)
	}
}

// randHex produces a short unique suffix so concurrent e2e runs don't
// alias on the same unaffiliated-thing id.
func randHex(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := randRead(buf); err != nil {
		t.Fatal(err)
	}
	const hex = "0123456789abcdef"
	out := make([]byte, len(buf)*2)
	for i, b := range buf {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0xf]
	}
	return string(out)
}
