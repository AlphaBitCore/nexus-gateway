# E33 S7 — Resilience: Rule Pack FK + pgx Cached Plan + Smoke Test

## Story

As a platform operator I need orphan `rule_pack_install` rows and Postgres prepared-statement cache drift to stop taking down the data-plane hook config reload, and a one-command smoke test that proves the end-to-end E33 wiring works against a live local stack.

## Scope

- `tools/db-migrate/schema.prisma` — add `@relation` from `rule_pack_install.boundHookId` to `"HookConfig".id` with `onDelete: Cascade`. Generate migration that includes a one-time `DELETE FROM rule_pack_install WHERE "boundHookId" NOT IN (SELECT id FROM "HookConfig")` to clean existing orphans before the FK takes effect.
- `packages/shared/policy/rulepack/enricher.go:Enrich` — change strict mode to **best-effort**. A single failed install ⇒ log ERROR + increment `nexus_rulepack_enrich_failure_total{hook,reason}` + skip the install. Only return an aggregate error when **every** install for a single hook failed (so the pipeline still has a working fallback).
- `packages/shared/policy/rulepack/store.go:LoadEffectiveSetsForHook` — wrap pgx error path so the typed `0A000` (cached plan must not change result type) triggers a single retry after `pgx.Conn.DeallocateAll(ctx)` on the conn pulled from the pool.
- `packages/shared/storage/configstore/pgx_options.go` (new) — sets `RuntimeParams: {"plan_cache_mode": "force_custom_plan"}` on the data-plane pgx pools that read DDL-mutable tables, eliminating the `0A000` class of failure entirely on those pools. Document in `docs/developers/workflow/conventions.md` why we accept the modest perf cost.
- `scripts/smoke-compliance-proxy-sse.sh` (new) — drives:
  1. compliance-proxy: real `chatgpt.com` SSE via local proxy, asserts dual pipeline + body capture in `traffic_event`/`traffic_event_payload`.
  2. ai-gateway: `/v1/chat/completions` SSE with the seeded VK (`nvk_…`), asserts same.
  3. (optional `--with-agent`) agent: replay through agent's intercept path.
  4. Verifies: no `audit/mq_writer: marshal failed` ERROR in the past 60 s; no `hook config reload failed` ERROR; NDJSON fallback file is empty.
- `packages/compliance-proxy/internal/audit/metrics.go` (new) — Prom counter `nexus_compliance_audit_drop_total{reason}` (already wired in S1 fallback path; this declares it).
- `packages/compliance-proxy/internal/proxy/policy_metrics.go` (new) — Prom histogram `nexus_compliance_streaming_chunks_per_stream` and gauge `nexus_compliance_buffer_bytes_high_watermark`.
- Tests: `enricher_test.go` for best-effort behavior; `pgx_options_test.go` validating the option string; smoke-test exit codes.

## Tasks

1. Add FK + cleanup migration; codegen.
2. Refactor `Enrich` to best-effort + new metric.
3. Add `pgx_options` defaults; apply across data-plane pgx pool factories.
4. Write `scripts/smoke-compliance-proxy-sse.sh`; document inputs (admin token + VK env vars) in script header.
5. Wire the new Prom metrics into the existing `/metrics` exposition.
6. Run the smoke locally on the dev stack; capture output as proof.

## Acceptance criteria

- Deleting a `HookConfig` row removes its `rule_pack_install` rows automatically (FK CASCADE); subsequent reload succeeds.
- Injecting a fake "missing pack" install on a hook with one valid install + one orphan ⇒ pipeline reload succeeds; one valid install enriches; metric `nexus_rulepack_enrich_failure_total{reason="install_load_failed"}` increments by 1.
- Running an offline `migrate dev` followed by a query that previously triggered `0A000` no longer errors — verified via integration test or manual smoke.
- `scripts/smoke-compliance-proxy-sse.sh` exits 0 against a freshly-seeded local stack.
