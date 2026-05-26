# tests/

Automated regression for Nexus Gateway. Master plan:
[`docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md`](../docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md).

## Quick start

```bash
cp tests/.env.test.example tests/.env.test
# Edit .env.test — at minimum, set NEXUS_TEST_VK to a real Nexus VK
# (research-all-models is fine for local dev).

# Run every layer (preflight + smoke + go + ui + python).
bash tests/run-all.sh --full

# Or just the fast deterministic tier.
bash tests/run-all.sh --quick

# Or one phase at a time.
bash tests/run-all.sh --phase smoke
bash tests/run-all.sh --phase go
```

The runner writes a unified markdown report to
`/tmp/nexus-test/test-all-<UTC>.md` with per-phase pass/fail and the last
40 lines of any failed phase's log.

## Layers (see master plan for full rationale)

| Layer | Where | Run with |
|-------|-------|----------|
| L1 smoke (Bash + curl + psql) | `tests/smoke/` | `bash tests/smoke/run-all.sh` |
| L1 Go integration | `tests/integration-go/` | `cd tests/integration-go && go test ./...` |
| L2 protocol (Python + openai/anthropic SDK) | `tests/e2e-python/protocol/` | `cd tests/e2e-python && uv run pytest protocol/` |
| L3 AI-judge (Python + Kimi 128k via Nexus VK) | `tests/e2e-python/ai_judge/` | `cd tests/e2e-python && uv run pytest ai_judge/` |
| L4 UI E2E (Playwright) | `tests/e2e-ui/` | `cd tests/e2e-ui && npx playwright test` |

## Prerequisites

The runner's preflight (`tests/lib/preflight.sh`) checks:

1. Postgres up + `Provider` table seeded
2. Hub `/health` returns 200
3. Control Plane `/api/admin/auth/login` accepts seeded admin credentials
4. AI Gateway `/v1/models` returns 200 with `NEXUS_TEST_VK` (only when set)
5. Compliance Proxy is listening on its port

Bring services up via `./scripts/dev-start.sh` before running tests.

## Shared helpers

| File | Purpose |
|------|---------|
| `lib/env.sh` | Loads `.env.test` and applies defaults; sourced first by every entry point. |
| `lib/assert.sh` | `pass` / `fail` / `assert_eq` / `assert_status` + summary at end. |
| `lib/db.sh` | `db_query` / `db_scalar` / `db_count` / `db_exists` — wraps `docker exec psql`. |
| `lib/auth.sh` | `cp_login` / `cp_curl` / `cp_curl_code` — Control Plane cookie auth. |
| `lib/http.sh` | `aigw_curl` / `hub_curl` + `wait_for_url`. |
| `lib/preflight.sh` | Verifies every dependency before a run. |

## Verification rule (binding)

Every test must verify reality through at least one of: HTTP response
assertion, DB cross-check, Prometheus counter delta, or AI-judge verdict.
Tests that only check "did the binary return without erroring" are not
acceptable. See master plan §6.
