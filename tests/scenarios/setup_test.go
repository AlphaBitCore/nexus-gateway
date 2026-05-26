// Scenario harness entry point. TestMain runs MustBeLocalTarget *before*
// any scenario can mutate state — the catalog §2 binding rule. Common
// fixtures (Env, pgx pool, scenario-local Cleanup) are exposed via the
// setupScenario helper so per-family files stay focused on business
// assertions, not boilerplate.
package scenarios_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	env, err := intg.LoadEnv()
	if err != nil {
		_, _ = os.Stderr.WriteString("scenario harness: cannot load tests/.env.<target>: " + err.Error() + "\n")
		os.Exit(1)
	}
	// Fail-closed env validation BEFORE any test body runs. Exits the
	// process on any localhost-allowlist violation.
	helpers.MustBeLocalTarget(env)
	// Block until the local dev environment is ready. Loops every 60 s
	// forever — the user explicitly authorized this pattern so a slow
	// `./scripts/dev-start.sh` (parallel build, Docker boot) doesn't
	// produce a noisy fail-fast run. Bypass with NEXUS_TEST_SKIP_PREFLIGHT=1.
	helpers.WaitForServices(env)
	os.Exit(m.Run())
}

// scenarioCtx is the per-test fixture every scenario asks for at the top
// of its body. Threading a single struct (rather than returning four
// values) keeps signatures stable as we add hub/proxy helpers later.
type scenarioCtx struct {
	Env     *intg.Env
	DB      *pgxpool.Pool
	Cleanup *helpers.Cleanup
}

// setupScenario resolves Env, opens the pgx pool, and binds a Cleanup
// registry to t. Skips the test (rather than failing) if NEXUS_TEST_VK
// is missing — use this when the scenario depends on an externally
// provided VK. Most scenarios should prefer setupScenarioNoVK and
// create their own VK via helpers.CreateMyVK so they are self-contained.
func setupScenario(t *testing.T) *scenarioCtx {
	t.Helper()
	sc := setupScenarioNoVK(t)
	if sc.Env.TestVK == "" || sc.Env.TestVK == "nvk_REPLACE_ME" {
		t.Skip("NEXUS_TEST_VK not set in tests/.env.local — skipping scenario")
	}
	return sc
}

// setupScenarioNoVK is the variant for self-contained scenarios that
// log in via OAuth+PKCE and mint their own VK at runtime (the §4 S-001
// pattern). No external NEXUS_TEST_VK is required.
func setupScenarioNoVK(t *testing.T) *scenarioCtx {
	t.Helper()
	env, err := intg.LoadEnv()
	if err != nil {
		t.Fatalf("load env: %v", err)
	}
	db, err := intg.DB(context.Background(), env)
	if err != nil {
		t.Fatalf("open DB pool: %v", err)
	}
	return &scenarioCtx{
		Env:     env,
		DB:      db,
		Cleanup: helpers.NewCleanup(t),
	}
}

// mustMarshal serialises v to JSON or fails the test. Keeps bodies of
// scenario tests free of `json.Marshal + nil check` boilerplate.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return b
}

// truncate caps a response body in error messages — full payloads blow
// up test logs without adding signal.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
