# Test harness architecture

The test harness is the layered regression suite under `tests/`, plus the
per-package unit tier beneath it. Each layer trades speed for fidelity: fast
deterministic checks at the base, full cross-service business flows and
LLM-judged behavior at the top. One orchestrator (`tests/run-all.sh`) runs them
in sequence and emits a single report.

This doc is the master plan that `tests/README.md` points to. It describes the
layers, the orchestrator, the shared library, the environment-file contract, and
the binding verification rule.

## 1. The test pyramid

| Layer | Lives in | Tech | Asserts |
| --- | --- | --- | --- |
| Unit | beside each Go package | `go test -cover` | One package's logic, ≥95% statements |
| L1 smoke | `tests/smoke/` | bash + curl + psql | A service answers + writes the expected row |
| L1 Go integration | `tests/integration-go/` | Go `testing` | Hook pipeline decisions against a live gateway |
| L2 protocol | `tests/e2e-python/protocol/`, `tests/e2e-python/sdk_compat/`, `tests/e2e-node/` | pytest + vitest + provider SDKs | Each ingress is wire-compatible with the real SDKs |
| L3 AI-judge | `tests/e2e-python/ai_judge/` | pytest + an LLM oracle | Semantic behavior a string match can't capture |
| L4 UI E2E | `tests/e2e-ui/` | Playwright | The admin UI works end-to-end in a browser |
| L5 scenarios | `tests/scenarios/` | Go `testing` | Coordinated business outcomes across services |

### Unit tier (the base)

Every Go package carries table-driven unit tests and must reach at least 95%
statement coverage, enforced by `scripts/check-go-coverage.sh` against
`scripts/.coverage-allowlist` (packages exempt only with a category and a written
rationale). This tier lives next to the code, not under `tests/`. The harness in
this doc sits on top of it.

### L1 smoke

`tests/smoke/` holds bash scripts (`test-hub.sh`, `test-control-plane.sh`,
`test-ai-gateway.sh`) that drive a service with `curl` and cross-check the result
with `psql`. `tests/smoke/run-all.sh` globs and runs every `test-*.sh`. This is
the fastest layer and the first to run.

### L1 Go integration

`tests/integration-go/` is its own Go module with shared `helpers/`. It exercises
the hook pipeline against a running gateway — for example, a PII prompt carrying
an SSN is rejected, a clean prompt is approved, and a bad virtual key is refused.

### L2 protocol

`tests/e2e-python/protocol/` uses the **real** OpenAI and Anthropic Python SDKs as
clients, so the tests fail if the gateway drifts from the wire shape those SDKs
expect. It covers the OpenAI Chat, Anthropic Messages, embeddings, and Responses
ingresses. Tests run under `pytest` with a per-test timeout.

Two further arms extend L2 to the full OpenAI SDK contract (AP-3):

- `tests/e2e-python/sdk_compat/` — 60 cases proving an **unmodified** `openai`
  Python SDK works with only `base_url` + `api_key` changed, across chat,
  streaming, tools (incl. parallel and streamed argument reassembly), structured
  output, vision, reasoning tokens, embeddings, and the named error scenarios.
  A `test_divergence.py` module pins the gateway's *deliberate* departures from
  api.openai.com, which are the evidence behind §8 of
  [`ingress-api.md`](../../services/ai-gateway/ingress-api.md) and the
  reader-facing matrix at `docs/users/api/openai-sdk-compatibility.md`.
- `tests/e2e-node/` — the Node SDK mirror (22 cases, one spec per scenario
  category). The exhaustive matrix stays on the Python side.

Both need a deployment with real upstream provider credentials: on
`target=local` every completion answers `502 PROVIDER_UNAVAILABLE`, so they run
with `--target stg`. Model selection is capability-gated against `GET /v1/models`
rather than pinned to model ids, and a missing capability skips with the unmet
constraint named — except embeddings, which fails, because the acceptance
criterion names it explicitly.

### L3 AI-judge

`tests/e2e-python/ai_judge/` uses an LLM as a test oracle for behavior that a
string assertion can't capture (for example, whether a response actually redacted
PII). The judge model is reached **through** the gateway's own
`/v1/chat/completions` using a Nexus virtual key, so every judge call also
dogfoods the gateway. A judge failure and a gateway failure surface the same way,
which keeps the layer honest.

### L4 UI E2E

`tests/e2e-ui/` runs Playwright specs against the Control Plane UI, with a
`global-setup.ts` that establishes an authenticated session before the specs run.

### L5 scenarios

`tests/scenarios/` is the top layer: Go tests that assert a coordinated outcome
across services, not a single endpoint — for example, *admin creates a virtual
key → the first `/v1/chat/completions` returns 200 → a `traffic_event` row lands →
a metric counter increments → an audit row appears*. Scenarios are numbered
(`S-NNN`) and catalogued in `tests/scenarios/00-catalog.md`, which maps every API
surface to its covering scenarios. The coverage target is every admin endpoint hit
by at least one scenario and every `/v1/*` ingress by at least three (happy, error,
edge). Shared `helpers/` provide admin setup, cleanup, metric reads, a per-run
preflight, and the safety guard described in [§5](#5-environment-file-contract-and-fail-closed-safety).

## 2. The orchestrator

`tests/run-all.sh` runs the layers in sequence and writes a unified markdown
report to `$NEXUS_TEST_LOG_DIR/test-all-<timestamp>.md` (defaulting to
`/tmp/nexus-test/`). Each phase logs to its own file; on failure the report
inlines the last lines of that log, and the run exits non-zero if any phase
failed.

Modes scale the run to the situation:

- `--quick` — smoke plus the single hello-world scenario. The fast tier that
  proves the toolchain wires up end-to-end.
- `--core` — smoke plus a curated set of high-priority scenarios (one per major
  family), sized to run on every commit without paying the full upstream-token
  cost.
- `--full` — every layer: smoke, Go integration, agent enrollment integration,
  Playwright, protocol, AI-judge, and the full scenario sweep.
- `--blocking` — the release-gate required-check profile: smoke plus the core
  one-per-family scenarios plus a bounded-model AI Gateway smoke (the model set
  named by `NEXUS_BLOCKING_MODELS`). Real upstream; a red verdict blocks.
- `--nightly` — the alert-only profile: the full scenario sweep plus the
  heavyweight tiers plus the full-surface `smoke-gateway --all-ingress`.
- `--phase <name>` — run only the named phase.
- `--no-preflight` — skip the preflight gate (debugging only).

After the layers, when an E2E coverage-matrix document is present the orchestrator
appends an informational snapshot of it — a count of covered, partial, and missing
cells — guarding on the file's existence and never failing the run on missing
cells; that closure is a separate pull-request gate. On macOS, a packet-filter
gap-closure phase runs only when explicitly opted in via an environment flag.

The two release-gate profiles draw their membership from the coverage matrix
([e2e-coverage-matrix.md](../../../specs/e2e-coverage-matrix.md)). The gate
verdict is **red** when our own behavior is wrong on a successful upstream call
(blocks the release), **amber** when an upstream `5xx` or timeout survives a
retry (alerts only), and **green** otherwise.

## 3. Preflight

`tests/lib/preflight.sh` is the gate every run passes first. It refuses to start
the suite unless every dependency is reachable:

1. PostgreSQL is reachable and the `Provider` table is seeded (a proxy for
   migrations + seed having run).
2. The Hub answers `200` on `/healthz`.
3. The Control Plane completes a real OAuth login (authorize → password →
   token) and the issued bearer works against a real admin endpoint
   (`GET /api/admin/providers`).
4. The Hub admin API answers `200` on `GET /api/hub/things` with the internal
   service token.
5. The AI Gateway answers `200` on `/v1/models` with a virtual key — checked only
   when a test virtual key is configured.
6. The Compliance Proxy is listening (any response other than a refused
   connection).

Bringing the stack up first (via the repo's dev-start script) is the prerequisite
for all of the above.

## 4. Shared library

`tests/lib/` is sourced by every entry point:

- `loadenv.sh` — the environment loader (see [§5](#5-environment-file-contract-and-fail-closed-safety)).
- `env.sh` — a back-compat shim that delegates to `loadenv.sh` for scripts that
  source it as their first line.
- `loadenv.py` — the Python equivalent for the pytest layers.
- `assert.sh` — `pass` / `fail` / `assert_eq` / `assert_status` and an end-of-run
  summary.
- `db.sh` — `db_query` / `db_scalar` / `db_count` / `db_exists`, wrapping `psql`
  inside the database container.
- `auth.sh` — `cp_login` / `cp_curl` / `cp_curl_code` for Control Plane auth.
- `http.sh` — `aigw_curl` / `hub_curl` and `wait_for_url`.
- `preflight.sh` — the dependency gate above.

## 5. Environment-file contract and fail-closed safety

Tests and prod-facing skills read configuration from `tests/.env.<target>` where
`target` is one of `local`, `dev`, or `prod`. `loadenv.sh` resolves the target
from its first argument, then `$NEXUS_TEST_TARGET`, then defaults to `local` — but
only on an interactive terminal. A non-interactive run (CI, redirected output)
**must** set `NEXUS_TEST_TARGET` explicitly; the loader refuses to default
otherwise. For each target it loads `.env.<target>.example` as defaults, then
`.env.<target>` (gitignored, operator-filled) on top, and process environment
variables set beforehand win over both.

The loader is fail-closed on target/host mismatch:

- `target=local` — every `NEXUS_*_URL` must reference `localhost` / `127.0.0.1`.
  A `.env.local` accidentally pointing at a remote host fails immediately, before
  any test runs.
- `target=prod` — `NEXUS_CP_URL` must **not** be loopback, catching a fresh
  `.env.prod` left with placeholder local values.

Scenarios mutate state (they create and delete virtual keys, routing rules, and
hooks; toggle kill switches; enroll devices), so they carry an additional guard.
`tests/scenarios/helpers/safety.go` enforces a closed hostname allowlist
(`localhost`, `127.0.0.1`, `::1`, `host.docker.internal`); any other host prints
the offending variable and exits before a single test runs. A non-interactive
scenario run requires `NEXUS_TEST_TARGET=local`, and the `cp_login` helper refuses
to drive a login against a non-loopback Control Plane URL. Scenarios are
local-only by construction; production reads go through a separate prod-login
path with its own token cache.

## 6. The verification rule

Every test must verify reality through at least one of: an HTTP response
assertion, a database cross-check, a Prometheus counter delta, or an AI-judge
verdict. A test that only checks "the binary returned without erroring" is not
acceptable. This is what makes a green run meaningful — each layer confirms an
observable effect, not merely the absence of a crash.

## 7. Test skills and standalone scripts

Several layers are also exposed as skills for targeted runs: a full-suite runner
over `run-all.sh`, a full-surface AI Gateway smoke
(`tests/scripts/smoke-gateway.py`, every model across every ingress), a compliance
proxy smoke, and synthetic-traffic adapters for the Cursor and Gemini-web
normalizers. The standalone scripts under `tests/scripts/` include the gateway
smoke, a virtual-key minter, and a coverage-gap reporter; `tests/manual/` holds
the synthetic Cursor and Gemini-web chat generators that back the adapter skills.

## References

- `tests/run-all.sh` — top-level orchestrator
- `tests/README.md` — quick start + layer table
- `tests/lib/` — env loader + shared bash/python helpers + preflight
- `tests/smoke/` — L1 bash + curl + psql smoke
- `tests/integration-go/` — L1 Go integration
- `tests/e2e-python/protocol/` — L2 protocol-compat (OpenAI / Anthropic SDKs)
- `tests/e2e-python/sdk_compat/` — L2 OpenAI SDK drop-in matrix (AP-3)
- `tests/e2e-node/` — L2 OpenAI Node SDK mirror (AP-3)
- `tests/e2e-python/ai_judge/` — L3 AI-judge
- `tests/e2e-ui/` — L4 Playwright UI E2E
- `tests/scenarios/` — L5 cross-service business-flow scenarios + `00-catalog.md`
- `tests/scripts/smoke-gateway.py` — full-surface AI Gateway smoke
- `scripts/check-go-coverage.sh`, `scripts/.coverage-allowlist` — unit coverage gate
