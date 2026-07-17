# Story A — continuous open-loop latency observatory

**Question answered:** how does latency change over time for one gateway/version at
low, fixed, **open-loop** load? Nothing about capacity or throughput — and never a
cross-gateway ranking (services roll independently; Story A publishes per-gateway
trends only).

## Built (this foundation)
- The **canonical run schema** (`../shared/schemas/run.schema.json`) every Story A run emits.
- The **mandatory validator** (`../scripts/validate_run.py`) — a Story A run is only
  published if it passes; otherwise it is quarantined under `results/quarantine/`.
- **Version-lock + provenance** (`../scripts/resolve_versions.py`,
  `../scripts/capture_provenance.py`) — exact pin per gateway + hardware/version record.
- `config/story-a.yaml` — open-loop 50 rps, 60s warmup, Fargate dedicated vCPU, gp3 disk spec.

## Deferred — infra lane (needs AWS; Tiebin's design)
Not built here, and intentionally **not stubbed**:
- `ecs/` — Fargate service + task defs (one gateway service at a time, dedicated
  in-VPC `nexus-mock-provider`, dedicated `nexus-loadtest` task).
- `loadtest/` — the open-loop fixed-arrival-rate driver wiring (client must NOT
  reduce offered load when a gateway slows; TTFT = first content delta).

The deferred runner MUST call `validate_run.py` as its final gate and
`capture_provenance.py` before load — that interface is fixed by this foundation.

`results/` holds published Story A runs; `results/quarantine/` holds invalidated
runs (with `.reasons.json`), retained for audit.
