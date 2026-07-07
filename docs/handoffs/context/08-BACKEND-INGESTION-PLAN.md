# 08 · Backend Ingestion Plan (chat-proposed, Jul 6) — status: PARTIALLY SUPERSEDED

> Captured from chat on 2026-07-06. This is the "Nexus Benchmark Dashboard — Backend
> Build Plan" proposal (artifact ingestion + normalization + static serving).
> **Read 02-BACKEND.md and 05-OBS-BACKEND-INTEGRATION.md first** — the live-run
> backend (obs-api, B0) is already built; this plan's V1 static layer largely
> exists as `scripts/export-benchmark-json.py` + `history/` + the portal.
>
> **What survives from this plan (adopt):** methodology-status enum, publish-gate
> validation, Agent-Gateway redaction flag, comparisons builder, governance fields.
> **What is superseded (do not build):** S3+CloudFront hosting (GitHub Pages is
> locked), Lambda/API-Gateway ingest (phone-home to obs-api is plan-of-record),
> a second normalizer parallel to the existing exporter (extend the exporter instead).

## Core idea (as proposed)

The backend is a benchmark artifact ingestion + serving layer: read rig artifacts
(SUMMARY.csv, raw JSON, benchmark-latest.json), normalize into a clean model,
serve to the site, later ingest phone-home metrics from the EC2 Arena. Honest by
construction — serves only what the rig produced.

V1 = static file backend (no server): `benchmark-results/` → normalizer →
`dist/data/{runs.json, latest.json, runs/<id>.json, comparisons/}`.
V2 = thin Go API when live Arena runs land.

## Methodology status enum (ADOPT — fold into exporter provenance)

```
draft                 → internal / incomplete
indicative            → directional, not statistically powered
externally_referenced → used in AlphaBitCore public materials
publishable           → James signed off
invalid               → known methodology issue (e.g. config mismatch)
superseded            → replaced by newer run
```

## Validation rules (ADOPT — publish gate before any public deploy)

- Required fields: run_id, benchmark_version, created_at, environment.instance_type,
  environment.layout, gateways.
- Methodology warnings that downgrade a run to `indicative`: configs_matched=false,
  caching_matched=false, same_machine=false, repeat_count unknown.
- Publish blockers: config mismatch, caching mismatch,
  agent_gateway_claims_public=true without James sign-off.
- Agent-Gateway guard: if `agent_gateway_claims_public` is false, strip/redact
  Agent Gateway rows from public-facing output (already enforced today by the
  exporter's EXCLUDE list — keep the code-diff-to-change property).

## Normalized run schema (proposed; align to existing site schema before adopting)

Key fields per run: run_id, benchmark_version, created_at, methodology_status,
publish_status, operator, environment {cloud, instance_type, layout, run_mode,
mock_provider, loadgen, region}, gateways[] {name, mode, image, version, rps,
ttft_p50/p90/p95_ms, e2e_p50/p90/p95_ms, itl_ms, error_rate, timeout_rate,
streaming_success_rate, hooks/compliance/audit/cache/pii_redaction flags,
traffic_events_captured, audit_loss_detected}, scenarios[], methodology
{same_machine, same_instance_type, configs_matched, caching_matched,
hooks_mode_labeled, repeat_count, statistically_powered,
independent_verification, notes}, artifacts {summary_csv, raw_results,
report_pdf, public_repo, git_commit, docker_image_tags}, caveats[],
agent_gateway_claims_public.

NOTE: the portal today renders `meta/gateways/tiers[].rows` (exporter schema).
Any adoption of the schema above must be a superset/mapping of that shape, not a
parallel format — one schema, one exporter.

## Comparisons builder (ADOPT later, low priority)

Prebuilt `comparisons/<left>-vs-<right>.json` files with deltas, winners, and a
governance note ("nexus-hooks-off runs with routing, VKs, quotas, audit; bifrost
is a thin proxy"). Client-side computation from runs.json is an acceptable
alternative — decide by payload size.

## Phone-home ingest endpoints (V2 — matches INTEGRATION.md; do not guess)

```
GET  /internal/pending-run       → loadgen pulls {runId, rps, durationSec, targets[]}
POST /internal/metrics           → loadgen streams --live-json samples
GET  /internal/runs/:id/status   → run status for orchestrator
```

Auth: shared token header, never public. These are Kash's next obs-api task,
gated on the phone-home-vs-SSM confirmation (plan of record: phone-home).

## Hard rules (as proposed — all consistent with program honesty rules)

- No fabricated numbers. No Agent-Gateway failure claims publicly before James
  signs off. Never mix live gateway traffic with benchmark traffic in one model.
- No Run() metric ingestion until the loadgen contract is pinned (it now is —
  INTEGRATION.md). No secrets in artifacts or API responses. Nothing marked
  `publishable` without James sign-off.

## Open questions from this plan → answered 2026-07-06, see ../program.md §Answers
