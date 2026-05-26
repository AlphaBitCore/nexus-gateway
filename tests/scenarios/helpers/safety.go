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
	"net/url"
	"os"
	"strings"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// allowedHosts is the closed allowlist of hostnames every NEXUS_*_URL must
// resolve to. Anything else (production, staging, peer-developer's box)
// causes MustBeLocalTarget to exit.
var allowedHosts = map[string]struct{}{
	"localhost":          {},
	"127.0.0.1":          {},
	"::1":                {},
	"host.docker.internal": {},
}

// forbiddenSubstrings is a defence-in-depth check: even if a host happens
// to resolve to 127.0.0.1 via /etc/hosts, a URL that mentions a known
// production domain is still wrong (probably a copy-paste from prod
// debugging) and must be rejected.
var forbiddenSubstrings = []string{
	"taskforce10x.com",
	"nexus.taskforce10x.com",
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
		for _, bad := range forbiddenSubstrings {
			if strings.Contains(raw, bad) {
				violations = append(violations, fmt.Sprintf("%s=%q: contains forbidden substring %q", name, raw, bad))
			}
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
