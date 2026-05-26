---
doc: test-harness-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Test Harness Architecture

> **Tier 2 architecture doc.** Read when adding a smoke / integration test, editing `tests/lib/`, or designing a new synthetic-test skill. The existing test-related skills (`/smoke-gateway`, `/test-all`, `/test-cursor-adapter`, etc.) are runners ON TOP of this harness; this doc is the substrate.

Nexus has several testing layers; each has a distinct purpose and SLA. The test harness is the shared library that makes them coherent.

---

## 1. The four layers

| Layer | Location | When run |
|---|---|---|
| **L0 — Go unit tests** | `*_test.go` next to source | `go test -race -count=1` on every commit; CI gate |
| **L1 — Smoke (HTTP shape + DB cross-check)** | `tests/lib/*.sh` + skill runners | On-demand (`/smoke-gateway`, `/test-all`) + post-deploy |
| **L2 — Protocol tests (synthetic providers)** | `tests/integration-go/` + skill runners | On adapter changes + pre-prod |
| **L3 — AI judge / Playwright UI** | `tests/integration-go/` + Playwright suites | Nightly + release-gate |
| **L5 — Scenario harness (admin × `/v1/*` × DB × metrics)** | `tests/scenarios/*_test.go` | `tests/run-all.sh --full` + on demand |

Each layer has different stability requirements. L0 must be deterministic. L1-L3 are network-dependent and rely on real prod-ish state.

**Coverage at the user-perspective level** (which user-facing capability is or isn't exercised end-to-end) is tracked in [`docs/developers/specs/e86-e2e-coverage-matrix.md`](../../../specs/e86-e2e-coverage-matrix.md). Endpoint-level coverage stays in [`tests/scenarios/00-catalog.md`](../../../../../tests/scenarios/00-catalog.md). The matrix's per-layer numeric targets live in §2 of the matrix file — single source of truth.

## 2. `tests/lib/` shared helpers

The single library that all shell-based tests reuse:

```
tests/lib/
  assert.sh      # assertion primitives (fail-with-message / equal / contains)
  auth.sh        # cp_login, cp_curl, cp_curl_full, cp_curl_code helpers
  db.sh          # psql wrappers; row count / specific cross-checks
  env.sh         # env helpers used by the loader
  http.sh        # curl wrappers with Bearer / retry / JSON-pretty
  loadenv.sh     # bash loader for tests/.env.<target>
  loadenv.py     # Python equivalent (used by e2e-python suites)
  preflight.sh   # service / port / DB readiness checks
```

Prometheus-delta tooling is currently inline in the runners (skills) that need it, not factored into a `prom.sh` helper. A test script sources `tests/lib/*.sh`, gets canonical helpers, and writes minimal glue code.

```bash
set -a && source tests/lib/loadenv.sh local && set +a
source tests/lib/auth.sh
source tests/lib/db.sh

cp_login
cp_curl /api/admin/analytics/summary | jq .
db_count "SELECT COUNT(*) FROM traffic_event WHERE provider='openai' AND emitted_at > now() - interval '5 min'"
```

## 3. The `cp_login` / `cp_curl` pattern

The canonical admin-API entry point. From `oauth-pkce-admin-auth-architecture.md` §6:

- `cp_login` runs the PKCE flow once; caches the bearer at `/tmp/nexus_test_token` (or `NEXUS_TOKEN_CACHE` env-override).
- `cp_curl /path` reads the cached bearer + makes the request.
- `cp_curl_full` returns the full response including headers.
- `cp_curl_code` returns just the HTTP status code.

Tests treat `cp_login` as idempotent — calling it multiple times is fine; only one actual flow runs.

## 4. The smoke skills layered on top

`.claude/skills/` houses runners that compose `tests/lib/`:

- `smoke-gateway` — full per-model + per-route smoke; auto-fix loop.
- `test-all` — preflight + L1 smoke + L1 Go integration + L2 protocol + L3 AI-judge + L4 Playwright UI. Single "did my change break something?" entry.
- `test-cursor-adapter`, `test-geminiweb-adapter` — per-adapter synthetic protocol smokes.
- `test-compliance-proxy` — end-to-end compliance-proxy smoke.

Each skill writes a Markdown report to `/tmp/nexus-test/<skill>-<utc>.md`.

## 5. L2 protocol tests (synthetic providers)

Adapter protocol tests live under `tests/integration-go/` (the `helpers/` subdirectory carries shared scaffolding; per-adapter tests live in sibling `*_test.go` files such as `hooks_test.go`). Synthetic provider servers (canned canonical bytes for OpenAI, Anthropic-shaped responses, Gemini-web batchexecute) are wired into individual tests; there is no dedicated `fake-providers/` directory today — each test stands up the listener it needs.

The adapter is tested by sending a request through the real ai-gateway → synthetic upstream → assert canonical response shape. Catches normalisation bugs without flaky upstream-provider dependencies.

## 6. L3 AI judge

Some tests need to verify "is this response actually a coherent answer?" — not just "did the request shape match expectations?". The AI judge spins up a separate model call (against a real, cheap provider) and asks "is this output reasonable for this prompt?".

Used sparingly because:

- Costs real money per test.
- Non-deterministic results need careful threshold-based pass/fail.

Best for testing prompt-cache behaviour (the response should be substantively the same across cache hits).

## 7. L4 Playwright UI

Browser-driven tests of the CP UI live under `tests/e2e-ui/specs/` (Playwright config at `tests/e2e-ui/playwright.config.ts`, global setup at `tests/e2e-ui/global-setup.ts`). Runs:

- Login flow.
- Navigate to each top-level section.
- Create a routing rule via UI.
- Verify the rule appears + is functional.
- Drive a few common workflows end-to-end.

Slow (~5 minutes per full run). Run nightly; gate release candidates.

## 8. Synthetic data — roadmap

A shared Go test-harness library (`HarnessNewTenant` / `HarnessSeedTraffic` / `HarnessEnableHook` / `HarnessCleanup`) does NOT exist today. Tests that need DB state currently use either:

- `tests/integration-go/helpers/` — per-area helpers (auth wiring, route-rule seeding) reused across the Go integration suites.
- Direct Prisma seeding via `tools/db-migrate` for fixtures shared with the prod-data baseline.

The roadmap item is to promote the recurring helpers into a single `testharness/` library once a third caller appears for the same combination. Until then, tests that need a fresh tenant should follow the pattern already in `helpers/`.

## 9. CI integration

```yaml
# .github/workflows/ci.yml (concept)
- name: L0 Go unit
  run: go test -race -count=1 ./...
- name: L1 smoke (post-deploy or scheduled only)
  run: ./tests/run-all.sh --layer=1
- name: L2 protocol tests
  run: ./tests/run-all.sh --layer=2
- name: L4 Playwright UI
  run: npx playwright test --config tests/e2e-ui/playwright.config.ts
```

L0 runs on every PR. L1-L4 run on specific triggers (label, schedule, post-deploy).

## 10. Adding a new test

Decide the layer:

- New unit test → `*_test.go` next to source. No harness changes.
- New smoke → write a bash script that sources `tests/lib/*`; add to `/test-all`.
- New protocol test → write Go test under `tests/integration-go/` reusing `helpers/` for shared scaffolding (synthetic upstreams are wired per-test for now — see §8).
- New AI-judge test → extend an existing AI-judge runner under `tests/e2e-python/ai_judge/`.
- New UI test → Playwright suite under `tests/e2e-ui/specs/`.

For new test areas significant enough to need their own skill (e.g., a new provider adapter), follow `test-cursor-adapter` / `test-geminiweb-adapter` as patterns.

<!-- 💡 harvest: the `tests/lib/` reusable-bash-library pattern (auth.sh + db.sh + http.sh + prom.sh) is well-established. Could be promoted to a documented public contract. Worth surfacing in CONTRIBUTING.md "how to write a smoke test" section; not a Cursor rule. -->

## 11. Sources

- `tests/lib/` — shared bash helpers (`assert.sh`, `auth.sh`, `db.sh`, `env.sh`, `http.sh`, `loadenv.sh`, `loadenv.py`, `preflight.sh`).
- `tests/integration-go/` — Go integration tests (`helpers/` + per-area test files; no `testharness/` library yet).
- `tests/e2e-ui/specs/` — Playwright UI tests.
- `tests/e2e-python/ai_judge/` — AI-judge runner.
- `tests/.env.local` — local dev env vars.
- `.claude/skills/smoke-gateway/`, `.claude/skills/test-all/`, etc. — runner skills.

## 12. Cross-references

- `oauth-pkce-admin-auth-architecture.md` §6 — `cp_login` / `cp_curl`.
- `provider-adapter-architecture.md` §8 — new-adapter smoke checklist.
- `prod-deploy` skill — uses smoke as the MANDATORY post-deploy gate.
- `testing.md` — top-level testing overview (sister doc).
