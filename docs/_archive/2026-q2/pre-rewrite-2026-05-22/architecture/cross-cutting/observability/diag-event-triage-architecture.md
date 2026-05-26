---
doc: diag-event-triage-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Diagnostics & Event Triage Architecture (E49)

> **Tier 2 architecture doc.** Read when touching `packages/shared/core/diag/`, `packages/nexus-hub/internal/observability/handler/diag/`, or `packages/control-plane/internal/observability/diag/`. Captures verbose telemetry on demand, surfaces diag events for triage, and applies silence rules so noisy false positives don't drown the signal.

The diag subsystem is the **operator's microscope**: enable per-Thing diag mode to capture verbose telemetry, surface every `slog` record at WARN+ on the `/infrastructure/errors` page, and apply silence rules so known-noise messages stay collapsed.

---

## 1. Pieces under `packages/shared/core/diag/`

| Piece | Where | What |
|---|---|---|
| Slog sink | `slog_sink.go` | Captures every `slog` record at the configured level and ships it to the Hub diag pipeline; produces the per-record `messageHash` used downstream for silence keying. |
| Multi-handler | `multi_handler.go` | Composes the slog sink with the underlying stdout / file handler so the diag sink runs alongside normal logging. |
| Reconnect buffer | `reconnect_buffer.go` | Bounded ring-buffer that holds diag records while the WebSocket back to Hub is down; drained on reconnect. |
| Recovery | `recovery.go` | Common `recover()` helper that captures the panic stack into a diag record before re-raising / converting to an HTTP 500. |
| Runtime introspection | `runtimeintrospect/` | Per-service HTTP surface exposed under `/runtime/*` — see `runtime-introspection-architecture.md`. |

There is no `diag/mode/` or `diag/triage/` subdirectory and no bucket classifier — diag mode is a binary "verbose telemetry on this Thing" flag, and triage is the operator looking at the raw events plus the silence registry.

## 2. Diag mode

Off by default (production-cost sensitive). When admin flips on:

- Verbose `slog` level (DEBUG).
- Per-phase timing on every traffic event.
- Body capture overrides (capture even on routes that normally don't).
- OTel span ratio bumped to 100%.

Enabled per Thing via a Cat A inline shadow key (fast-flip). Auto-expires after admin-configured `until` timestamp; the handler caps the window at `maxDiagModeDuration = 24h` (`packages/control-plane/internal/infrastructure/infra/diagmode.go:39`) to prevent leaving diag mode on indefinitely.

Activation: CP UI → Infrastructure → Diagnostic Mode → select Thing → duration → confirm. Audit event recorded.

## 3. Event triage surface

Diag events are raw `slog` records (level + message + structured fields) shipped from every service through the slog sink. The triage surface is the `/infrastructure/errors` page (CP UI): every `WARN`/`ERROR` record is listed, grouped by `messageHash` (a stable hash of the message template + selected fields), with a count, last-seen timestamp, and a "Silence" affordance.

There is no per-bucket classifier — operators do the triage by reading the list and silencing the message hashes they recognise. Recurring anomaly patterns instead live as **alert rule aggregators** (`alerting-architecture.md` §3), which is the right place for "this metric crossed a threshold" detection.

## 4. Silence rules

Operators can silence specific records by message hash + level. The Prisma model is `DiagSilence` (`tools/db-migrate/schema.prisma:2280`):

```
messageHash  -- the stable per-template hash assigned by the slog sink
level        -- "WARN" | "ERROR" (silenced records still write to the underlying log file)
silencedBy   -- NexusUser.id of the operator who applied the silence
silencedAt   -- when the silence was applied
expiresAt    -- nullable; NULL = permanent silence; populated => auto-clears past the timestamp
reason       -- free-text justification (nullable)
```

There is no bucket / scope concept — silence keys directly off the message hash. Silenced records still land on disk; only the `/infrastructure/errors` triage list collapses them and the alert pipeline drops them.

Permanent silences (`expiresAt = NULL`) are allowed because the message-hash key is precise; operator review of "known noise" stays in scope without aging back into the alert feed.

## 5. Support bundle

Generate via admin UI (Infrastructure → Crashes / Errors) or Agent UI (Health & Diagnostics → Generate Support Bundle).

Contents:

- Last 24h of logs (compressed).
- Current effective config (redacted: no provider plaintext, no JWKS private keys).
- Recent traffic_event rows (last 100, sanitised).
- Service / Thing metadata (version, uptime, host info).
- Platform info (OS, arch, kernel version).

Output: `.tar.gz` saved locally; admin downloads and ships to support.

The agent bundle goes through `platform.DefaultPaths().CacheDir` (cross-ref `feedback_agent_platform_paths_abstraction`). The server-side bundle lives in `/tmp/nexus-support-bundle-<id>.tar.gz` and is auto-cleaned after admin downloads.

## 6. Anomaly detection lives in the alert pipeline

Trend-based anomaly detection (provider error-rate spikes, hook latency regressions, traffic drops) does NOT happen in the diag subsystem. It is the job of the alert aggregators (`alerting-architecture.md` §3); the canonical built-in catalog lives in `packages/nexus-hub/internal/alerts/engine/rules/builtin.go`. Diag stays focused on its narrow concern: capturing slog records + surfacing them for human triage.

## 7. Operator workflow

Typical incident triage:

1. Alert fires (e.g., `model.rate_limited_responses`).
2. Operator opens CP UI → Alerts → expand row → see sample request_ids.
3. Click request_id → land on the unified audit timeline.
4. If many similar events → check Infrastructure → Diagnostics → bucket view.
5. Decide: real incident → page someone OR known issue → apply silence rule.
6. If unclear → enable diag mode on the affected Thing for an hour, watch the verbose telemetry.

The diag mode + triage buckets cut the mean-time-to-diagnose for repeat patterns.

## 8. Cost concerns

Diag mode is opt-in BECAUSE it's expensive:

- DEBUG-level logging is ~10× the volume of INFO.
- 100% OTel sampling can saturate trace exporters.
- Body capture on all routes burns spillstore.

The 24h `until` cap is the safety belt — operators sometimes forget to turn it off.

<!-- 💡 harvest: the "max 24h" / "auto-expire" pattern is repeated across diag, kill switch, silence rules. Could become a shared `bounded-temporal-state` design pattern in the shared package. Not urgent; revisit if a 4th use case emerges. -->

## 9. Sources

- `packages/shared/core/diag/` — slog sink, multi-handler, reconnect buffer, recovery helper, runtime introspection.
- `packages/nexus-hub/internal/observability/handler/diag/` — Hub-side diag bridge + bundle generation.
- `packages/control-plane/internal/observability/diag/` (+ `infrastructure/infra/diagevents.go`, `diag_silences.go`, `diagmode.go`) — admin CRUD on diag mode + silences and the `/infrastructure/errors` listing.
- `docs/developers/specs/e49/e49-diag-event-triage-and-silence.md` — original requirements.

## 10. Cross-references

- `alerting-architecture.md` — anomaly-detection aggregators live there, not here.
- `audit-pipeline-architecture.md` — audit and diag are sibling pipelines; agent operational events flow through diag, not audit.
- `kill-switch-architecture.md` §4 — sibling bounded-emergency pattern.
- `metrics-rollup-architecture.md` — metric trends that feed anomaly detection.
- `docs/users/features/cp-ui/infrastructure.md` — Diag Mode admin page.
