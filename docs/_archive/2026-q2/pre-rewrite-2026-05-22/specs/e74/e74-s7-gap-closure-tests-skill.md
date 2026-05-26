# E74-S7 — Gap-Class Closure Tests + `/test-macos-pf-agent` Skill

> **Epic**: 74 — macOS pf-intercept replacement of NETransparentProxyProvider
> **Story**: 7
> **Status**: Planning
> **Date**: 2026-05-21
> **FR mapping**: FR-7.1, FR-7.2, FR-7.3, FR-7.4, FR-7.5, FR-7.6
> **Source decisions**: DEC-002 (redirect all TCP 443; `domain.Engine` decides in-daemon),
> DEC-012 (`domain.Engine` shared between Compliance Proxy and Agent — tests verify
> both services see identical `inspect | passthrough | deny` decisions for the same
> `interception_domain` rows)

---

## 1. User Story

**As a** Nexus macOS agent operator who has flipped `interceptMode` to `pf`,
**I want** an automated acceptance test that exercises each of the five inherent
NE-architecture gap classes under the new pf path,
**so that** I can confirm the structural gaps are closed before approving the
`pf`-mode rollout to the install base, and re-run the same gate on every
subsequent prod release without manual setup.

---

## 2. Tasks

### T7.1 — Create `tests/agent/gap_closure/` Go test harness

Create the directory `tests/agent/gap_closure/` with a top-level
`gap_closure_test.go` that:

- Declares `package gap_closure_test` with build tag `//go:build darwin`.
- Reads all test configuration from `tests/.env.local` (or the target selected
  by `NEXUS_TEST_TARGET` env var) using the project's standard `bootenv` loader.
  Fails closed with a clear error if `tests/.env.<target>` is absent or if a
  required key is missing. Required keys: `NEXUS_AGENT_LISTENER_ADDR` (default
  `127.0.0.1:13443`), `NEXUS_DB_DSN` (Postgres DSN for `traffic_event` queries),
  `NEXUS_PROMETHEUS_ADDR` (metrics endpoint, default `http://localhost:9100`).
- Provides shared helpers:
  - `waitForTrafficEvent(t, traceID string, timeout time.Duration) TrafficEventRow` —
    polls the DB (via `pgx`) for a `traffic_event` row whose `trace_id` matches,
    up to `timeout`. Fails the test if none arrives.
  - `queryNormalizedContent(t, traceID string) NormalizedContent` — queries
    `traffic_event_normalized` for the matching row and returns
    `request_normalized` + `response_normalized` parsed JSON.
  - `assertPrometheusCounter(t, addr, metric string, minDelta float64)` — snapshots
    the counter at test start and end, asserts the delta ≥ `minDelta`.
- Each gap-class test (T7.2–T7.6) lives in its own `_test.go` file inside this
  package and uses these shared helpers.

Coverage target: ≥95% on the helper package per CLAUDE.md binding (helpers
are testable via mock DB + mock Prometheus; the gap-test files themselves are
integration-only and listed in `.coverage-allowlist` under category E
"network-infra-bound" with rationale "live pf + daemon required").

### T7.2 — Gap 1: raw-socket interception test (`gap1_raw_socket_test.go`)

**FR-7.1.** Write `TestGap1RawSocket` (build tag: `darwin`):

1. Compile `tests/agent/gap_closure/testfixtures/raw_socket_client/main.go` — a
   minimal Go binary (no NE-aware code) that opens `net.Dial("tcp",
   "api.openai.com:443")`, writes a valid TLS ClientHello by hand (or uses
   `crypto/tls.Client` with a fresh `tls.Conn`), and sends one small HTTP/1.1
   GET. The binary accepts `--trace-id <id>` and embeds it as a custom HTTP
   header `X-Nexus-Trace-ID: <id>` so the test can find the resulting row.
2. Generate a unique `traceID := "gap1-" + ulid.New()`.
3. Record `t0 := time.Now().UTC()` and snapshot `nexus_agent_inspected_total`
   from Prometheus.
4. Run the compiled binary via `os/exec`, capturing stdout + stderr. The binary
   is expected to get an HTTP 400 or 200 from the upstream (exact code irrelevant
   — the pf path must have intercepted regardless of upstream response).
5. Call `waitForTrafficEvent(t, traceID, 15*time.Second)`. Assert:
   - `source = 'agent'`
   - `endpoint_type` is non-empty (the normalizer should detect `chat` or
     `completion` for `api.openai.com`)
   - `request_normalized` is not NULL
6. Assert `assertPrometheusCounter` shows ≥1 increment on `nexus_agent_inspected_total`.

Fixture source: `tests/agent/gap_closure/testfixtures/raw_socket_client/main.go`.
The fixture binary must NOT import any Nexus package — it is intentionally a
naive outbound process to prove pf catches raw dials.

### T7.3 — Gap 2: QUIC-fallback without bundle list (`gap2_quic_fallback_test.go`)

**FR-7.2.** Write `TestGap2QUICFallback` (build tag: `darwin`):

1. Confirm precondition: the test machine has `quicFallbackUIDs` configured in
   `agent_settings` with a uid entry covering the current test process's uid
   (resolved from `os.Getuid()`). Skip (not fail) if the precondition is absent,
   printing "SKIP: test uid not in quicFallbackUIDs — see T7.3 setup note".
2. Attempt a UDP/443 connection to a QUIC-capable host (configurable via
   `NEXUS_GAP2_TARGET_HOST`, default `chatgpt.com`) using a raw UDP dial. This
   simulates the first phase of QUIC happy-eyeballs.
3. Assert the UDP connection is blocked / receives no QUIC ServerHello within
   3 seconds (pf rdr rule intercepted UDP/443 for this uid and the daemon has
   no QUIC relay — it closes UDP flows per FR-1.4).
4. Open a TCP/443 connection to the same host (the happy-eyeballs TCP fallback).
   Assert that `waitForTrafficEvent` finds a row for this TCP flow.
5. Assert `source = 'agent'`, `target_host` contains the configured host.

Open question (defer to Code phase): the exact mechanism by which the daemon
signals "UDP blocked" may differ from "TCP captured" — the row structure for
blocked UDP flows is not yet specified in the SDD.

### T7.4 — Gap 3: fail-open content capture rate under load (`gap3_load_test.go`)

**FR-7.3.** Write `TestGap3ContentCaptureRate` (build tag: `darwin`):

1. Accept a concurrency dial: `NEXUS_GAP3_CONCURRENCY` (default `10`) and
   `NEXUS_GAP3_DURATION_S` (default `60`). These match the FR-7.3 target of
   "10 concurrent IDE sessions × 5 minutes" scaled down to a CI-feasible envelope.
2. Spawn `concurrency` goroutines. Each goroutine loops for `duration` seconds,
   sending a minimal HTTPS GET to `NEXUS_GAP3_TARGET_HOST` (default
   `api.openai.com`) via a direct `net.Dial` (bypasses any HTTPS_PROXY — forces
   pf to catch the flow). Each request carries a unique `X-Nexus-Trace-ID`.
3. Collect all trace IDs into a `[]string`. After the load window, wait up to
   30 s for all rows to land in DB.
4. Query:
   ```sql
   SELECT COUNT(*) AS total,
          COUNT(CASE WHEN ten.request_normalized IS NOT NULL THEN 1 END) AS with_content
   FROM traffic_event te
   LEFT JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
   WHERE te.trace_id = ANY($1)
     AND te.source = 'agent'
   ```
5. Assert `with_content / total >= 0.95`. Log the actual ratio to test output
   for observability.
6. If the assertion fails, print the trace IDs of rows missing content for
   operator diagnosis (do not fail immediately — collect all misses, then assert).

### T7.5 — Gap 4: per-hop latency observability (`gap4_latency_test.go`)

**FR-7.4.** Write `TestGap4LatencyObservability` (build tag: `darwin`).
This test is **observability-only** — it reports a number but does NOT gate on it.

1. Send 50 sequential HTTPS GETs through the pf path to `NEXUS_GAP4_TARGET_HOST`
   (default `api.openai.com`), each measuring:
   - `connTime`: time from `net.Dial` to TCP ESTABLISHED (kernel-level; captured
     via `DialContext` with timestamps around the Dial call).
   - `ttfb`: time from first byte written to first byte received.
2. Compute p95 of `ttfb - connTime` across 50 samples. This is the user-space
   overhead attributable to pf redirect + daemon listener + SNI peek.
3. Query the NE-baseline from `NEXUS_GAP4_NE_BASELINE_P95_MS` env (set by the
   operator from a prior NE-path measurement, or left unset). If set, log:
   `"Gap 4 p95 overhead: <value> ms (NE baseline: <baseline> ms, ratio: <pct>%)"`.
   If unset, log: `"Gap 4 p95 overhead: <value> ms (no NE baseline provided)"`.
4. Always call `t.Log(...)` with the measured values. Never call `t.Fail()` or
   `t.Error()` — this test must always pass; it is a measurement, not a gate.
5. The skill's report template (T7.9) includes a dedicated **Gap 4** section that
   reproduces this log output for the operator.

### T7.6 — Gap 5: Chrome helper-process attribution (`gap5_attribution_test.go`)

**FR-7.5.** Write `TestGap5HelperProcessAttribution` (build tag: `darwin`):

1. Precondition: `NEXUS_GAP5_CHROME_PATH` is set to a Chrome.app path on the
   test machine. Skip (not fail) if unset.
2. Launch Chrome (`exec.Command(chromePath, "--headless=new", "--disable-gpu",
   "--no-sandbox", `--dump-dom", "https://chatgpt.com/")`) which will spawn one
   or more helper processes (`Google Chrome Helper`, `Google Chrome Helper
   (Renderer)`, etc.).
3. Wait for up to 20 s for Chrome helper traffic to produce ≥10 `traffic_event`
   rows with `source = 'agent'` and `target_host LIKE '%openai.com%'` (or
   `'%chatgpt.com%'`).
4. For each row, check `source_bundle`:
   - `com.google.Chrome` (or `com.google.Chrome.canary`) → counts as **correctly
     attributed** (parent bundle, not helper).
   - `com.google.Chrome.helper` or any Helper variant → counts as **attribution
     gap** (helper miscounted).
   - Empty → counts as **attribution gap** (libproc lookup failed).
5. Assert `correctly_attributed_count / total >= 0.9` (NFR-9 threshold: ≥90%).
6. Log per-row attribution outcome for operator diagnosis on failure.
7. Kill the Chrome process after the assertion.

### T7.7 — Cross-service consistency check (`gap_cross_service_consistency_test.go`)

**DEC-012 verification.** Write `TestDomainEngineConsistency` (build tag: `darwin`):

1. Read a set of test domains from `NEXUS_CONSISTENCY_TEST_DOMAINS` (comma-separated,
   e.g. `"api.openai.com,api.anthropic.com,internal.corp.example"`) or fall back
   to a hardcoded set of 5 well-known AI provider hosts.
2. For each domain, query the `interception_domain` table directly and derive the
   expected decision (`inspect | passthrough | deny`) by calling
   `packages/shared/policy/domain.Engine.Evaluate` in-process (same code the
   agent's pf listener uses per DEC-002 + DEC-012).
3. Query the compliance proxy's runtime decision for the same domain by making
   a CONNECT request through `NEXUS_CP_PROXY_ADDR` (default `localhost:3128`):
   a TCP CONNECT that deliberately sends an empty body after CONNECT so the
   proxy's decision (accept / reject / reset) can be observed without actually
   bumping TLS.
4. Map the compliance proxy's TCP-level response (`200 Connection Established`,
   `403 Forbidden`, immediate reset) to `inspect | passthrough | deny`.
5. Assert that for every test domain the agent-side decision equals the cp-side
   decision. Any divergence is a test failure — print the domain, expected
   decision, and observed cp decision.

Open question (defer to Code phase): the exact TCP-level signal the compliance
proxy emits for `passthrough` vs `deny` (close vs reset vs 403) is not yet
documented in the SDD for S2; this test's decision-mapping table must be
finalised when S2's interface contract is locked.

### T7.8 — Create `.claude/skills/test-macos-pf-agent/` skill

**FR-7.6.** Create the skill directory and two files:

#### `.claude/skills/test-macos-pf-agent/SKILL.md`

Frontmatter and content following the exact shape of `test-compliance-proxy/SKILL.md`:

- `name`: `test-macos-pf-agent`
- `description`: one-paragraph summary covering: runs the five gap-class closure
  tests (Gap 1 raw socket, Gap 2 QUIC fallback, Gap 3 content capture rate,
  Gap 4 latency observability, Gap 5 helper attribution) plus the DEC-012
  cross-service consistency check against a local macOS dev machine running
  the pf-mode agent daemon. Reads config from `tests/.env.local`. Produces
  a Markdown report at `/tmp/test-macos-pf-agent-<UTC-ts>.md`.
- `user-invocable: true`
- Trigger keywords: `test macos pf, gap closure, pf agent smoke, verify pf
  interception, /test-macos-pf-agent`.
- **Inputs section**: document every env var the runner reads:
  `NEXUS_AGENT_LISTENER_ADDR`, `NEXUS_DB_DSN`, `NEXUS_PROMETHEUS_ADDR`,
  `NEXUS_GAP2_TARGET_HOST`, `NEXUS_GAP3_CONCURRENCY`, `NEXUS_GAP3_DURATION_S`,
  `NEXUS_GAP3_TARGET_HOST`, `NEXUS_GAP4_TARGET_HOST`,
  `NEXUS_GAP4_NE_BASELINE_P95_MS` (optional), `NEXUS_GAP5_CHROME_PATH`
  (optional), `NEXUS_CONSISTENCY_TEST_DOMAINS` (optional).
- **Workflow section**: mirrors `test-compliance-proxy` workflow:
  preflight → t0 snapshot → run gap tests → collect results → render report.
- **When something fails** section: authorise fix / build / restart of the
  agent daemon only (not Hub / CP / AI Gateway). Same shape as the compliance
  proxy skill's fix loop.
- **Report shape** section: documents the per-gap subsection titles exactly as
  written in T7.9 below.
- **What this skill does NOT do**: does not test Windows / Linux pf paths;
  does not run the AI Gateway smoke; does not seed `interception_domain` rows;
  does not test adapter normalisation correctness (that is `test-cursor-adapter`'s job).

#### `.claude/skills/test-macos-pf-agent/runner.sh`

Bash runner script that:

1. Loads `tests/.env.${NEXUS_TEST_TARGET:-local}` via `set -a; source ...; set +a`.
   Exits 1 with an error message if the file does not exist.
2. Validates that `NEXUS_DB_DSN` is set (fail-closed).
3. Determines `REPORT_PATH=/tmp/test-macos-pf-agent-$(date -u +%Y%m%dT%H%M%SZ).md`.
4. Runs:
   ```bash
   cd "$(git rev-parse --show-toplevel)"
   go test -v -count=1 -timeout=300s \
     -run 'TestGap|TestDomainEngine' \
     ./tests/agent/gap_closure/... \
     2>&1 | tee /tmp/test-macos-pf-agent-raw.log
   EXIT=$?
   ```
5. Calls a Go helper (`tests/agent/gap_closure/cmd/render_report/main.go`,
   see T7.9) to parse the raw log and emit the structured Markdown report to
   `$REPORT_PATH`.
6. Prints `$REPORT_PATH` on the last line of stdout.
7. Exits with the same code as the `go test` invocation.

### T7.9 — Report template (`cmd/render_report/main.go`)

Create `tests/agent/gap_closure/cmd/render_report/main.go` — a small Go
program that reads the `go test -v` output from stdin (or a file argument)
and writes a structured Markdown report to stdout. Report shape:

```markdown
# macOS pf-Agent Gap-Closure Report — <UTC timestamp>

## Environment
- Agent listener: <NEXUS_AGENT_LISTENER_ADDR>
- DB DSN: <host:port/db only — password redacted>
- Prometheus: <NEXUS_PROMETHEUS_ADDR>
- macOS version: <sw_vers -productVersion>
- interceptMode: pf (assumed — this skill does not verify)
- Run duration: <elapsed>

## Summary
| Gap | Test | Result | Notes |
|---|---|---|---|
| Gap 1 — raw socket | TestGap1RawSocket | PASS / FAIL | |
| Gap 2 — QUIC fallback | TestGap2QUICFallback | PASS / SKIP / FAIL | |
| Gap 3 — content capture rate | TestGap3ContentCaptureRate | PASS / FAIL | actual: <ratio>% |
| Gap 4 — latency (observability only) | TestGap4LatencyObservability | OBS | p95=<N>ms NE baseline=<M>ms |
| Gap 5 — helper attribution | TestGap5HelperProcessAttribution | PASS / SKIP / FAIL | <N>/10 correctly attributed |
| Cross-service consistency | TestDomainEngineConsistency | PASS / FAIL | 0 divergences |

## Gap 1 — Raw Socket
<go test output section for TestGap1RawSocket>
traffic_event row: id=<id> source=agent endpoint_type=<type>

## Gap 2 — QUIC Fallback
<go test output section for TestGap2QUICFallback>
UDP blocked: <yes/skipped>  TCP captured: <yes/no>

## Gap 3 — Content Capture Rate Under Load
<go test output section for TestGap3ContentCaptureRate>
Total flows: <N>  With content: <M>  Rate: <pct>%  Threshold: 95%

## Gap 4 — Per-Hop Latency (Observability Only — Not a Gate)
p95 user-space overhead: <N> ms
NE baseline: <M> ms (or "not provided")
Ratio pf/NE: <pct>% (target: ≤80%)

## Gap 5 — Helper-Process Attribution
<go test output section for TestGap5HelperProcessAttribution>
Correctly attributed: <N>/10  Rate: <pct>%  Threshold: 90%

## Cross-Service Consistency (DEC-012)
Domains tested: <list>
Divergences: 0 (PASS) / <N> (FAIL with table)

## Result
**PASS** / **FAIL** — <N> of 6 arms green (<Gap 4 is observability-only, always green>)
```

### T7.10 — Wire into `tests/run-all.sh` (conditional)

Add a conditional block to `tests/run-all.sh`:

```bash
if [[ "$(uname)" == "Darwin" ]] && [[ "${NEXUS_RUN_MACOS_PF_TESTS:-false}" == "true" ]]; then
  echo "==> Running macOS pf gap-closure tests"
  bash .claude/skills/test-macos-pf-agent/runner.sh
fi
```

The `NEXUS_RUN_MACOS_PF_TESTS` guard keeps the default CI run (Linux) unaffected.
macOS developers opt-in by exporting the variable in their `tests/.env.local`.

### T7.11 — Env-file binding: add required keys to `tests/.env.local` template

Per CLAUDE.md "Test/skill env files live under `tests/.env.<target>`", add the
new keys documented in T7.8 to:

- `tests/.env.local.example` (committed to repo as the template).
- `docs/developers/workflow/local-dev-debugging.md` "Test / skill env files"
  section — one row per new key in the documented table.

Keys that contain secrets (e.g. `NEXUS_DB_DSN`) are already in the template
from prior stories; confirm they are not duplicated.

---

## 3. Acceptance Criteria

Each criterion is observable and independently verifiable:

| ID | Criterion | How to verify |
|---|---|---|
| AC-1 | `TestGap1RawSocket` passes on the macOS 26 dev machine with `interceptMode=pf`. | `go test -v -run TestGap1RawSocket ./tests/agent/gap_closure/...` exits 0; `traffic_event` row confirmed in DB. |
| AC-2 | `TestGap2QUICFallback` passes (or skips with documented precondition message) on the macOS 26 dev machine. | `go test -v -run TestGap2QUICFallback ./tests/agent/gap_closure/...` exits 0. |
| AC-3 | `TestGap3ContentCaptureRate` passes with content-capture ratio ≥95% at default concurrency/duration settings. | `go test -v -run TestGap3ContentCaptureRate ./tests/agent/gap_closure/...` exits 0; ratio logged in output. |
| AC-4 | `TestGap4LatencyObservability` always exits 0; the report section contains a numeric p95 value in milliseconds. | `go test -v -run TestGap4LatencyObservability ./tests/agent/gap_closure/...` exits 0 regardless of measured value. |
| AC-5 | `TestGap5HelperProcessAttribution` passes with ≥9/10 helper flows attributed to the parent bundle, or skips with the documented skip message when Chrome is absent. | `go test -v -run TestGap5HelperProcessAttribution ./tests/agent/gap_closure/...` exits 0. |
| AC-6 | `TestDomainEngineConsistency` passes with zero divergences between agent-side and cp-side decisions for the test domain set. | `go test -v -run TestDomainEngineConsistency ./tests/agent/gap_closure/...` exits 0; "0 divergences" in output. |
| AC-7 | `.claude/skills/test-macos-pf-agent/runner.sh` runs end-to-end, reads `tests/.env.local`, and produces a Markdown report at `/tmp/test-macos-pf-agent-*.md` with all six sections populated. | `bash .claude/skills/test-macos-pf-agent/runner.sh` completes; report file exists; contains "## Gap 1", "## Gap 2", ..., "## Cross-Service Consistency", "## Result". |
| AC-8 | Report's **Result** line reads `PASS` when all five gap-class tests and the consistency check pass. | Visual inspection of report `## Result` line. |
| AC-9 | `tests/run-all.sh` runs the pf skill when `NEXUS_RUN_MACOS_PF_TESTS=true` on macOS; does not run (and does not error) on Linux or when the flag is absent. | `uname` guard confirmed in script; CI (Linux) pipeline unaffected. |
| AC-10 | `tests/.env.local.example` and `local-dev-debugging.md` env-file table are updated with all new keys (T7.11). | `grep NEXUS_GAP1 tests/.env.local.example` returns a line; doc table updated. |

---

## 4. Interface Contract

### 4.1 Skill CLI

```
bash .claude/skills/test-macos-pf-agent/runner.sh [--dry-run] [--gap <N>]
```

| Flag | Default | Behaviour |
|---|---|---|
| (none) | — | Run all six test functions. |
| `--dry-run` | — | Print planned test matrix and env summary; exit 0 without running tests. |
| `--gap <N>` | — | Run only `TestGap<N>*` (1–5) or `TestDomainEngineConsistency` (pass `consistency`). |

The flags are passed as `go test -run <pattern>` filters inside the runner.

### 4.2 Report shape

Mandatory sections (skill fails if any section is absent from the rendered report):

```
# macOS pf-Agent Gap-Closure Report — <timestamp>
## Environment
## Summary          ← table with one row per gap + consistency
## Gap 1 — Raw Socket
## Gap 2 — QUIC Fallback
## Gap 3 — Content Capture Rate Under Load
## Gap 4 — Per-Hop Latency (Observability Only — Not a Gate)
## Gap 5 — Helper-Process Attribution
## Cross-Service Consistency (DEC-012)
## Result           ← "PASS" or "FAIL" + counts
```

### 4.3 DB assertions

All DB queries target the tables and columns produced by Stories S1–S4:

| Query | Table | Key columns asserted |
|---|---|---|
| traffic_event lookup by trace_id | `traffic_event` | `source`, `endpoint_type`, `target_host`, `trace_id` |
| normalised content check | `traffic_event_normalized` | `request_normalized IS NOT NULL` |
| attribution check | `traffic_event` | `source_bundle` |

No writes. No `DELETE`, `UPDATE`, or `INSERT` statements. Tests observe existing
rows only (per CLAUDE.md binding: tests must only touch own data via prefixed
trace IDs they generated).

### 4.4 Prometheus assertions

Counter name: `nexus_agent_inspected_total` (expected to be introduced in S2).
Gap 3 additionally checks `nexus_agent_passthrough_total` to compute the
inspect / passthrough split.

Open question (defer to Code phase): the exact Prometheus metric names for the
pf listener are not yet finalised in S2's interface contract.

---

## 5. Dependencies

Stories that MUST be implemented and their acceptance criteria met before this
story's harness can run end-to-end:

| Story | Title | Required output |
|---|---|---|
| S1 | pf anchor + rdr rules (`pfintercept/pfrules`) | Daemon installs `nexus-agent/transparent` anchor and redirects TCP 443 to `127.0.0.1:13443`. |
| S2 | Daemon loopback listener (`pfintercept/listener`) | Listener accepts redirected connections, calls `BumpFlow`, writes `traffic_event` rows with `source='agent'`. |
| S3 | `libproc` PID attribution (`pfintercept/pidlookup`) | `source_bundle` populated in `traffic_event` rows for flows from bundle apps. |
| S4 | Fail-open invariants + panic auto-recovery | Daemon startup clears stale rules; SIGTERM handler removes anchor before exit. |

**Note**: T7.7 (cross-service consistency) additionally requires the Compliance
Proxy to be running locally on `localhost:3128` — that is an existing dev-stack
service, not an E74 dependency.

---

## 6. Out of Scope

- **VM-based reproducible test environments** — running the harness inside a
  macOS virtual machine (Apple Silicon VM, Tart, etc.) for fully reproducible
  CI isolation is E75's responsibility (E75 S3 / S4 / S5 re-verify on the pf
  path). E74-S7 targets the developer's own macOS dev machine.
- **CI integration of the gap-closure skill** — the skill is manual / on-demand
  for E74. Automated CI scheduling (nightly macOS runner) is a follow-up; it
  requires a dedicated macOS CI runner with root access, which the current
  GitHub Actions setup does not provide.
- **Windows and Linux** — the `tests/agent/gap_closure/` package uses build tag
  `darwin`; tests do not compile on other platforms.
- **AI Gateway adapter correctness** — this story tests the interception path
  (flows captured, rows written, attribution correct). Adapter normalisation
  quality (confidence scores, message extraction, model field accuracy) is
  tested by `/test-cursor-adapter` and the AI Gateway smoke (`smoke-gateway`).
- **Performance benchmarking beyond FR-7.4** — the latency test (T7.5)
  measures p95 overhead as an observability number, not a load benchmark.
  Sustained throughput, CPU/memory profiling, and backpressure behaviour under
  >100 concurrent flows are post-E74 work.
- **E86 e2e-coverage-matrix entry** — NFR-13 in the Requirements mandates that
  a row for `/test-macos-pf-agent` is added to
  `docs/developers/specs/e86-e2e-coverage-matrix.md` in the E86 worktree. That
  cross-repo edit is a coordination step listed as a task in the E74 main PR
  checklist but executed in the E86 worktree.

---

## 7. References

- **Requirements §FR-7** — `docs/developers/specs/e74-macos-pf-intercept.md`
  §FR-7 (FR-7.1 through FR-7.6): the five gap-class closure requirements and
  the skill-productisation requirement that this story implements.
- **DECISIONS DEC-002** — `docs/developers/specs/e74/DECISIONS.md` §DEC-002:
  redirect all TCP 443; domain engine decides in-daemon. Tests in T7.2–T7.4
  rely on this architecture (all test flows land on the same listener and are
  evaluated by `domain.Engine`).
- **DECISIONS DEC-012** — `docs/developers/specs/e74/DECISIONS.md` §DEC-012:
  `domain.Engine` is shared between Compliance Proxy and Agent. T7.7's
  consistency check directly verifies the invariant this decision protects.
- **Reference skill: `.claude/skills/test-cursor-adapter/SKILL.md`** — shape
  reference for a single-scenario synthetic test skill (minimal, trace-ID
  driven, DB cross-check pattern). T7.8's SKILL.md adopts the same "When to
  use / Inputs / Workflow / Verification / What wrong looks like" structure.
- **Reference skill: `.claude/skills/test-compliance-proxy/SKILL.md`** — shape
  reference for a multi-scenario smoke skill with report template, fix/build/
  restart authorization, and red-flag section. T7.8's SKILL.md adopts the same
  report-template format, runner.sh pattern, and operator guardrails.
- **CLAUDE.md "Test/skill env files live under `tests/.env.<target>`"** — the
  binding that forces all skill config into `tests/.env.local` and prohibits
  hardcoded credentials. T7.11 documents compliance.
- **NFR-13 / E86 e2e-coverage-matrix** — `docs/developers/specs/e74-macos-pf-intercept.md`
  §NFR-13: the `/test-macos-pf-agent` skill must appear in
  `docs/developers/specs/e86-e2e-coverage-matrix.md` so future features cannot
  land without exercising the pf acceptance gate.
