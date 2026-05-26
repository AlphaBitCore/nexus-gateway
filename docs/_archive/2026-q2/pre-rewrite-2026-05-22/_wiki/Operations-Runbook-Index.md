# Operations Runbook Index

The runbooks under [`docs/operators/ops/runbooks/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/) are the canonical step-by-step procedures for production operations. Each runbook is self-contained with prerequisites, numbered steps, and verification queries. This index catalogs every runbook with a one-line summary and the conditions that trigger it.

---

## Alert management

**[`alerts.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/alerts.md)** — How to acknowledge and resolve alerts, create notification channels, and triage missed pages.

Covers the three-state model (`firing → acknowledged → resolved`), the `cp_curl`-based acknowledge/resolve API, channel types (webhook / Slack / email / PagerDuty), and a five-step "why didn't I get paged?" decision tree including the `alert_dispatch` query pattern.

**When to open:** An alert fires and requires operator action; a notification channel is suspected misconfigured; `alert_dispatch` rows show `success = false`.

---

## Compliance proxy smoke test

**[`compliance-proxy-smoke.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/compliance-proxy-smoke.md)** — End-to-end probe of the compliance proxy MITM pipeline using real provider traffic.

Covers all eight stages: prerequisites, obtaining provider keys, verifying interception domains, enabling payload capture, snapshotting baselines, running four provider probes (OpenAI, Anthropic, DeepSeek, Moonshot), auditing `traffic_event` + Prometheus + NATS JetStream, and cleanup.

**When to open:** After changes to the compliance proxy or its audit pipeline; before cutting a compliance proxy release; during incident triage when the audit pipeline appears stuck.

---

## Ops metrics + diagnostics smoke test

**[`ops-metrics-smoke-test.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/ops-metrics-smoke-test.md)** — End-to-end probe of the ops-metrics and diagnostics pipeline for all five services.

Covers sample flow verification (`metric_ops_raw`), `staticInfo` identity payload, 1-hour rollup closure, agent diag pipeline (synthetic ERROR + FATAL injection), diag-mode window lifecycle, retention policy, and Control Plane UI surface verification.

**When to open:** After changes to `packages/shared/runtime/opsmetrics/`, Hub ops-rollup jobs, or the CP `/api/admin/ops-metrics` / `/api/admin/diag-events` handlers; when Status or Recent Errors pages render empty.

---

## Production deploy — database and data changes

**[`prod-deploy-data-changes.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/prod-deploy-data-changes.md)** — Single source of truth for every database change applied at each production deploy.

Structured as a living checklist: Section 0 (pre-flight baseline), Section 1 (Prisma migrations), Section 2 (required data inserts), Section 3 (historical data fixes), Section 4 (intentionally no-action items), Section 5 (post-deploy verification). Reset after each deploy.

**When to open:** Every production deploy. The `prod-deploy` skill reads this file before applying binaries.

---

## Performance — AI Gateway `traffic_event` deep dive

**[`perf-2026-05-20-nexus-traffic-event.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md)** — Gateway-side latency decomposition using `traffic_event` phase columns.

Covers the `latency_ms → usOverhead + upstream_ttfb + upstream_total` decomposition, per-scenario phase percentiles from the benchmark run (W-01 through W-04), the `hit_inflight` singleflight coalescer pattern, and the analytics API endpoints used to reproduce the analysis.

**When to open:** Investigating AI Gateway latency; benchmarking performance; understanding the singleflight coalescer behavior.

---

## Performance — Nexus vs Bifrost benchmark

**[`perf-2026-05-20-nexus-vs-bifrost.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-vs-bifrost.md)** — k6 end-to-end comparison of Nexus AI Gateway vs Bifrost under identical workloads.

Covers methodology, load profiles (W-01 through W-04), side-by-side throughput and latency numbers, the two mechanisms that drive the difference (response cache + singleflight coalescer), and steps to reproduce on a fresh EC2 run.

**When to open:** Evaluating competitive positioning; reporting on cache and throughput characteristics; reproducing benchmark numbers.

---

## Agent rollout rings (version pinning)

**[`r-version-pinning-rollout-rings.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/r-version-pinning-rollout-rings.md)** — How to implement canary → beta → stable rollout rings for Desktop Agent auto-updates.

Covers the three-ring model using `agent_settings.autoUpdateChannel` and device group priority cascade, `cp_curl` commands to assign rings, promotion via device-tag edits, and verification via Hub `desired` state query.

**When to open:** Rolling out a new agent build to a subset of devices before fleet-wide release; validating a build on canary devices.

---

## Valkey migration

**[`e61-valkey-migration.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/e61-valkey-migration.md)** — Step-by-step production cutover from Redis 7 to Valkey 8 with the `valkey-search` module.

Covers preflight checks, RDB snapshot, stop/swap/restart sequence, module verification (`FT.CREATE` / `FT.SEARCH` / `FT.DROPINDEX`), Prometheus metric validation, rollback procedure, and common pitfalls.

**When to open:** Upgrading the cache tier from Redis 7 to Valkey 8; verifying `valkey-search` is loaded after a container restart.

---

## Adding a new runbook

Follow the pattern in [`Recipe Adding A Runbook`](Recipe-Adding-A-Runbook). Every runbook must include: a one-line "when to run" declaration, prerequisites table, numbered steps, verification queries, and a cleanup section if it makes temp changes.

---

## Canonical docs

- [`docs/operators/ops/runbooks/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/) — all runbooks directory
- [`backup-dr.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/backup-dr.md) — backup and disaster recovery procedures
- [`monitoring.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/monitoring.md) — Prometheus metrics and health endpoints reference

**Adjacent wiki pages**: [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet) · [Operations Backup Restore](Operations-Backup-Restore) · [Operations Migrations On Prod](Operations-Migrations-On-Prod) · [Operations Logs Metrics Traces](Operations-Logs-Metrics-Traces) · [Operations FAQ](Operations-FAQ)
