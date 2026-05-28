// Package helpers — scenario-test-specific helpers, layered on top of the
// shared tests/integration-go/helpers package.
//
// safety.go is the *first* helper any scenario calls. It enforces the
// target-environment isolation rules documented in
// tests/scenarios/00-catalog.md §2 (binding). The scenario harness mutates
// state (creates/deletes VKs, routing rules; activates kill-switches;
// inserts audit rows) — a stray pointer at production would be a
// destructive incident. We fail-closed, never fail-open.
package helpers

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// prodSafeE2E is flipped true by MustBeLocalOrProdSafeE2E when the operator
// explicitly opts into the prod SAFE-E2E mode (NEXUS_TEST_TARGET=prod +
// NEXUS_PROD_SAFE_E2E=1). In that mode the harness may run against prod, but
// CPDoJSON / CPDoWithKey refuse any MUTATING method aimed at a shared/global
// config surface (see sharedStateMutationPrefixes). Reads and own-object CRUD
// (create → modify → delete the SAME object the test made, by its own unique
// id) are permitted — that is data-safe by construction. Process-global; set
// once in TestMain before any test runs.
var prodSafeE2E bool

// IsProdSafeE2E reports whether the harness is in prod safe-e2e mode.
func IsProdSafeE2E() bool { return prodSafeE2E }

// sharedStateMutationPrefixes are admin path prefixes that mutate GLOBAL /
// shared singletons or existing fleet objects — NOT a test's own created
// resource. In prod safe-e2e mode a mutating method (anything but GET/HEAD) to
// any of these is refused at the choke point so a scenario cannot change live
// prod policy/config (kill-switch, passthrough, settings, cache singletons,
// node overrides, config-sync, alert rules). Own-object collections
// (/api/admin/providers, /routing-rules, /my/virtual-keys, /hooks, …) are
// intentionally absent: creating/modifying/deleting one's own object is safe.
var sharedStateMutationPrefixes = []string{
	"/api/admin/settings/",
	"/api/admin/passthrough/",
	"/api/admin/kill-switch",
	"/api/admin/emergency",
	"/api/admin/semantic-cache/",
	"/api/admin/extract-cache/",
	"/api/admin/cache/",
	"/api/admin/config-sync/",
	"/api/admin/nodes/",
	"/api/admin/ip-access",
	"/api/admin/streaming-compliance",
	"/api/admin/alerts/",
}

// GuardProdSafeE2E is the single choke point every admin call routes through
// (CPDoJSON / CPDoWithKey). It returns a non-nil error when prod safe-e2e mode
// is active and (method, path) would mutate a shared/global surface — the
// defense-in-depth that makes "safe e2e against prod" enforceable rather than
// mere convention. Returns nil for GET/HEAD, for any path outside the
// shared-state denylist (own-object CRUD), and whenever the mode is off (the
// local path is untouched).
func GuardProdSafeE2E(method, path string) error {
	if !prodSafeE2E {
		return nil
	}
	if method == http.MethodGet || method == http.MethodHead {
		return nil
	}
	for _, p := range sharedStateMutationPrefixes {
		if strings.HasPrefix(path, p) {
			return fmt.Errorf(
				"prod-safe-e2e: refusing %s %s — mutating a shared/global config surface "+
					"against prod is blocked; only reads and own-object CRUD are permitted",
				method, path)
		}
	}
	return nil
}

// allowedHosts is the closed allowlist of hostnames every NEXUS_*_URL must
// resolve to. Anything else (production, staging, peer-developer's box)
// causes MustBeLocalTarget to exit.
var allowedHosts = map[string]struct{}{
	"localhost":          {},
	"127.0.0.1":          {},
	"::1":                {},
	"host.docker.internal": {},
}

// CheckLocalTarget validates the resolved Env against the §2 binding
// rules and returns a slice of violation strings — empty means the
// target is acceptable. Split out from MustBeLocalTarget so the rules
// are unit-testable without invoking os.Exit.
func CheckLocalTarget(env *intg.Env) []string {
	violations := []string{}

	checkURL := func(name, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s=%q: unparseable URL (%v)", name, raw, err))
			return
		}
		host := u.Hostname()
		if _, ok := allowedHosts[host]; !ok {
			violations = append(violations, fmt.Sprintf("%s=%q: host %q not in localhost allowlist", name, raw, host))
		}
		// HTTPS is allowed only against localhost (dev TLS cert).
		if u.Scheme == "https" {
			if host != "localhost" && host != "127.0.0.1" {
				violations = append(violations, fmt.Sprintf("%s=%q: https only allowed against localhost/127.0.0.1", name, raw))
			}
		}
	}
	checkURL("NEXUS_HUB_URL", env.HubURL)
	checkURL("NEXUS_CP_URL", env.CPURL)
	checkURL("NEXUS_AI_GW_URL", env.AIGwURL)
	checkURL("NEXUS_PROXY_URL", env.ProxyURL)
	checkURL("NEXUS_UI_URL", env.UIURL)

	// DB DSN safety: host must be local, port should not be the well-known
	// production Postgres port (5432) on a non-localhost host.
	if env.PGHost != "localhost" && env.PGHost != "127.0.0.1" {
		violations = append(violations, fmt.Sprintf("NEXUS_PG_HOST=%q: must be localhost or 127.0.0.1", env.PGHost))
	}
	if env.PGPort == "5432" && env.PGHost != "localhost" && env.PGHost != "127.0.0.1" {
		violations = append(violations, fmt.Sprintf("NEXUS_PG_PORT=5432 on host=%q: refusing — looks like a production DSN", env.PGHost))
	}

	return violations
}

// MustBeLocalTarget validates the resolved Env against the §2 binding
// rules. On any violation, it prints the offending var and exits 1 before
// any test runs. Idempotent within a process.
//
// Call from TestMain. The harness will not run a single assertion
// without this gate.
func MustBeLocalTarget(env *intg.Env) {
	violations := CheckLocalTarget(env)
	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "scenario harness refuses to start — target environment is not local:")
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  •", v)
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Scenarios mutate state (VKs, routing rules, kill-switch, audit rows).")
		fmt.Fprintln(os.Stderr, "Edit tests/.env.local so every NEXUS_*_URL points at localhost.")
		fmt.Fprintln(os.Stderr, "For prod debugging use the /prod-login skill — never scenarios.")
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	confirmTarget(env)
}

// MustBeLocalOrProdSafeE2E is the TestMain entry guard. It preserves the
// fail-closed local default and adds ONE narrow, explicit escape hatch: a prod
// SAFE-E2E mode for the curated read-only / own-object-lifecycle subset.
//
//   - NEXUS_TEST_TARGET != "prod": identical to MustBeLocalTarget — every URL
//     must be loopback or the harness exits. Unchanged for local/dev.
//   - NEXUS_TEST_TARGET == "prod": refuses unless NEXUS_PROD_SAFE_E2E=1 is ALSO
//     set (an accidental prod target still hard-fails). With the opt-in, it
//     inverse-validates that the CP URL is genuinely non-loopback (catches a
//     half-edited .env.prod), flips prodSafeE2E on, and prints a loud banner.
//     From that point CPDoJSON/CPDoWithKey refuse any mutating method aimed at
//     a shared-state surface, so only reads + own-object CRUD can run on prod.
func MustBeLocalOrProdSafeE2E(env *intg.Env) {
	if os.Getenv("NEXUS_TEST_TARGET") != "prod" {
		MustBeLocalTarget(env)
		return
	}
	if os.Getenv("NEXUS_PROD_SAFE_E2E") != "1" {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "scenario harness refuses to start — NEXUS_TEST_TARGET=prod without NEXUS_PROD_SAFE_E2E=1.")
		fmt.Fprintln(os.Stderr, "Scenarios mutate state by default and must never run against prod.")
		fmt.Fprintln(os.Stderr, "Only the curated read-only / own-object-lifecycle subset may run against prod,")
		fmt.Fprintln(os.Stderr, "and only with NEXUS_PROD_SAFE_E2E=1 (run-all.sh --target prod sets it).")
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
	if u, err := url.Parse(env.CPURL); err == nil {
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			fmt.Fprintln(os.Stderr, "prod-safe-e2e: NEXUS_CP_URL is loopback ("+env.CPURL+") — not a prod target. Aborting.")
			os.Exit(1)
		}
	}
	prodSafeE2E = true
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "════════════════════════════════════════════════════════════════════")
	fmt.Fprintln(os.Stderr, " PROD SAFE-E2E MODE — running against:", env.CPURL)
	fmt.Fprintln(os.Stderr, "   Mutations to shared/global config surfaces are hard-blocked at")
	fmt.Fprintln(os.Stderr, "   CPDoJSON/CPDoWithKey. Run only the curated safe -run set.")
	fmt.Fprintln(os.Stderr, "════════════════════════════════════════════════════════════════════")
	fmt.Fprintln(os.Stderr)
}

// confirmTarget requires the operator to explicitly acknowledge the
// resolved target before any test runs.
//
//   - TTY: prints the target and prompts y/N. Anything other than y/Y aborts.
//   - Non-TTY (CI, `go test` piped to a file): refuses to proceed unless
//     NEXUS_TEST_TARGET=local is set in the environment, so a redirected
//     run cannot silently skip the prompt.
func confirmTarget(env *intg.Env) {
	if os.Getenv("NEXUS_TEST_TARGET") == "local" {
		// Operator has acknowledged via env var — typically CI.
		return
	}

	isTTY := isStdinTTY()
	if !isTTY {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "scenario harness refuses to start — non-interactive run without acknowledgement.")
		fmt.Fprintln(os.Stderr, "Set NEXUS_TEST_TARGET=local in the environment to acknowledge the local target.")
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Scenario tests will mutate state against:")
	fmt.Fprintln(os.Stderr, "  Hub  :", env.HubURL)
	fmt.Fprintln(os.Stderr, "  CP   :", env.CPURL)
	fmt.Fprintln(os.Stderr, "  AIGW :", env.AIGwURL)
	fmt.Fprintln(os.Stderr, "  Proxy:", env.ProxyURL)
	fmt.Fprintln(os.Stderr, "  DB   :", env.PGHost+":"+env.PGPort+"/"+env.PGDB)
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "Target environment looks LOCAL. Proceed? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" {
		fmt.Fprintln(os.Stderr, "Aborted by operator.")
		os.Exit(1)
	}
}

// isStdinTTY reports whether os.Stdin is connected to a terminal. Used to
// pick the interactive prompt vs the env-var acknowledgement path.
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
