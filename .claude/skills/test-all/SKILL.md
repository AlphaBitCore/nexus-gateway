---
name: test-all
description: >
  Run the full Nexus Gateway end-to-end test program (preflight + L1 smoke +
  L1 Go integration + L2 protocol + L3 AI-judge + L4 Playwright UI) via
  tests/run-all.sh and surface the unified markdown report. The single
  "did my change break something" entry point — covers ~75 business flows
  across all five services, each verified by HTTP shape + DB cross-check
  + Prometheus delta or AI-judge verdict. Trigger keywords: test all,
  run regression, smoke everything, full regression, e2e tests,
  regression sweep, /test-all. No required inputs; reads
  tests/.env.local for VKs and admin credentials. Output: a Markdown
  report at /tmp/nexus-test/test-all-<UTC-timestamp>.md plus a fan-out
  summary in this conversation.
user-invocable: true
---

# Test All

Drives `tests/run-all.sh` end-to-end and reports back. This is the
recommended entry point after any non-trivial code change — it covers
every layer of the test program (`docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md`) and produces
one report you can paste into a PR or hand to a reviewer.

## When to use

- User typed `/test-all`, `/test-all --quick`, or asked something like
  "run all tests", "regression check", "test everything", "smoke the
  whole system".
- Right before declaring a multi-service change done.
- After a service restart or config change, to confirm everything is
  still wired up.

Don't use this for narrowly-scoped debugging — pick a single layer (L1
smoke, L1 Go integration, L4 UI) and run that test directly.

## Inputs

| Arg | Required | Notes |
|---|---|---|
| `--quick` | no | Preflight + L1 smoke only (≈ 30 s, no LLM calls). Default mode. |
| `--full` | no | All phases including L3 AI-judge and L4 Playwright (≈ 3–5 min, costs ~5 Kimi calls). |
| `--phase <name>` | no | Run only one phase: `smoke`, `go`, `ui`, `protocol`, `ai-judge`. |
| `--no-preflight` | no | Skip preflight (use only when debugging the runner itself). |

If the user did not pass a flag, default to `--full`. The cost of one full
run (~2 ¢ in Kimi tokens, ~5 minutes wall) is worth it as a green/red
signal; downgrade to `--quick` only if the user asked for fast feedback.

## Prerequisites

Verify before starting (the runner's own preflight will also check; we
front-run these so failures surface in chat, not in a buried log):

1. `tests/.env.local` exists and `NEXUS_TEST_VK` is not the placeholder
   `nvk_REPLACE_ME`. If missing, instruct the user to copy
   `tests/.env.local.example` and set `NEXUS_TEST_VK` to a real Nexus VK
   (the `research-all-models` VK is fine for local dev; query the DB
   if needed: `SELECT name, "keyPrefix" FROM "VirtualKey" WHERE enabled
   = true AND "vkType" = 'application' LIMIT 5;`).
2. Local services running. Quick check:
   `lsof -nP -iTCP:3001,3040,3050,3060 -sTCP:LISTEN`. All four ports
   must show a listener. If any are missing, ask the user whether to
   start them (`./scripts/dev-start.sh` is the canonical bootstrap)
   rather than launching them silently.
3. Postgres + Redis containers up: `docker ps --filter
   name=nexus-postgres --filter name=nexus-redis --format '{{.Names}}'`.

If any of those fail, surface the specific gap and stop — don't run a
test suite that's guaranteed to fail in preflight.

## Workflow

1. Verify prerequisites (above). If gap, surface and stop.
2. Run `bash tests/run-all.sh <flags>` from the repo root, capturing
   stdout. The runner is non-interactive and exits non-zero on any
   phase failure.
3. Read the report file the runner printed at the end (path is
   `Report: /tmp/nexus-test/test-all-<UTC>.md`). The report has one
   `## Phase: <STATUS>` block per phase plus a "Failures" section if
   anything is red.
4. Compose the chat reply with this structure (concise — the user has
   the full report path for detail):

   ```
   ## Test run <UTC>

   - Preflight: ✅ / ❌
   - Phase 1 (L1 smoke): X/Y ✅
   - Phase 2 (Go integration): X/Y ✅ / SKIPPED
   - Phase 3 (Playwright): X/Y ✅ / SKIPPED
   - Phase 4 (AI-judge): X/Y ✅
   - Phase 5 (protocol): X/Y ✅ / SKIPPED
   - Total wall: <s>

   <on failure: 1-2 lines per failed phase pointing at the log path>

   Full report: /tmp/nexus-test/test-all-<UTC>.md
   ```

5. If a phase failed: open `/tmp/nexus-test/<UTC>/<phase>.log`, find
   the first non-pass line (✗ in shell scripts, FAILED in pytest,
   FAIL in `go test`), include its name + reason in the summary.

6. Do NOT ask the user follow-up questions when reporting — they ran
   `/test-all` to get a status, not to start a debugging conversation.
   If something is broken, give them enough info to triage and let them
   drive the next step.

## Verification discipline

The skill itself does no assertion logic — `tests/run-all.sh` and the
phase scripts handle that. The skill's job is to be the one-command
green/red signal and to surface failures readably. If you find yourself
adding "let me also curl X to verify" steps to this skill, that
verification belongs in the underlying phase script instead.

## Cost note

A full run does ~5 Kimi 128k completions (Phase 4 AI-judge), which
dogfoods our own AI Gateway through the configured Nexus VK. Per master
plan §10, this stays under $0.10 per run at current Moonshot pricing.
Quick mode does no LLM calls.
