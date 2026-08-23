package scenarios_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// requireProviderSeeded is what a scenario calls when a request came back
// ROUTING_NO_MATCH — no provider could serve it.
//
// These call sites used to read `t.Skipf("no provider seeded locally")`. That
// is not an environmental fact: Provider.json ships seven providers, so on a
// seeded database the absence of any usable provider is a regression, and
// skipping means the scenario goes quiet precisely when it should go red. The
// helper tells the two apart — never seeded → skip, naming `npm run seed`;
// seeded but nothing usable → fail, saying so.
func requireProviderSeeded(t *testing.T, arm, body string) {
	t.Helper()
	ctx := context.Background()
	env, err := intg.LoadEnv()
	if err != nil {
		t.Fatalf("%s: ROUTING_NO_MATCH and the test env will not load: %v", arm, err)
	}
	pool, err := intg.DB(ctx, env)
	if err != nil {
		t.Fatalf("%s: ROUTING_NO_MATCH and the database is unreachable, so we cannot "+
			"tell a missing seed from a routing regression: %v (body=%q)", arm, err, body)
	}
	helpers.RequireSeeded(t, ctx, helpers.PoolRowCounter(pool), "Provider",
		"no provider able to serve "+arm+" (body="+body+")")
}

// requireHookSeeded is the same distinction for a hook the seed ships.
//
// HookConfig.json ships pii-outbound-scanner, so on a seeded database its
// absence is a regression — a rename, a broken seed, a changed query — and the
// scenarios that depend on it must go red rather than quietly stop covering it.
func requireHookSeeded(t *testing.T, pool *pgxpool.Pool, name string, lookupErr error) {
	t.Helper()
	ctx := context.Background()
	helpers.RequireSeeded(t, ctx, helpers.PoolRowCounter(pool), "HookConfig",
		"the "+name+" hook (lookup error: "+fmt.Sprint(lookupErr)+")")
}

// requireLiveGateway handles "no ai-gateway Thing has been seen recently".
//
// S-076 already treats this condition as a hard failure, and its comment says
// why: ./scripts/dev-start.sh always runs Hub, CP, AI Gateway and
// compliance-proxy, all of which register and refresh last_seen_at, so zero
// fresh rows means the heartbeat path is broken rather than "nothing is
// connected". Two other scenarios skipped on the identical condition, which
// means the same regression would be reported by one test and hidden by two.
//
// The split is the same one the seed helper makes: a thing table with no rows
// at all is an environment that was never brought up (skip, and say so); rows
// present but none fresh is the regression S-076 names (fail).
func requireLiveGateway(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	// Under prod safe-e2e the pool is NOT a prod connection: prod Postgres is
	// bound to localhost on the prod host and tests/.env.prod carries no
	// DATABASE_URL, so sc.DB points at whatever local database the environment
	// happens to have. Counting `thing` rows there answers a question about a
	// different deployment, and the seed helper's "never seeded — run npm run
	// seed" is then advice to seed the WRONG database. Say what is actually
	// true instead, and skip.
	if helpers.IsProdSafeE2E() {
		t.Skipf("%s needs the deployment's own database, and prod Postgres is not "+
			"reachable from here (bound to localhost on the prod host; "+
			"tests/.env.prod carries no DATABASE_URL) — this scenario is "+
			"local-only, not a regression", what)
	}
	ctx := context.Background()
	helpers.RequireSeeded(t, ctx, helpers.PoolRowCounter(pool), "thing",
		"no ai-gateway Thing seen recently, so "+what+" cannot run — this is the "+
			"heartbeat regression S-076 fails on, not an absent stack")
}

// requireOpenAIProvider skips when the dev vault has no enabled OpenAI provider
// with a credential — the precondition the cross-format scenario names.
//
// It replaces an UNCONDITIONAL t.Skip. That is the failure mode worth naming: a
// skip whose reason is a real precondition, but which never re-checks it, keeps
// the scenario off forever, including after someone fixes exactly the thing the
// message asked for. The L5 landing rule wants a cited precondition; a cited
// precondition that is never evaluated is a "we'll fix later" wearing its
// clothes.
func requireOpenAIProvider(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	env, err := intg.LoadEnv()
	if err != nil {
		t.Skipf("cannot load the test env to check for an OpenAI provider: %v", err)
	}
	pool, err := intg.DB(ctx, env)
	if err != nil {
		t.Skipf("cannot reach the database to check for an OpenAI provider: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM "Provider" p
		JOIN "Credential" c ON c.provider_id = p.id
		WHERE p.adapter_type = 'openai' AND p.enabled = true
	`).Scan(&n); err != nil {
		t.Skipf("cannot tell whether an OpenAI provider is configured: %v", err)
	}
	if n == 0 {
		t.Skip("no enabled OpenAI provider with a credential in this environment — " +
			"cross-format ingress needs one specifically (the moonshot path other " +
			"scenarios use is not a substitute). Add one and this scenario runs itself.")
	}
}
