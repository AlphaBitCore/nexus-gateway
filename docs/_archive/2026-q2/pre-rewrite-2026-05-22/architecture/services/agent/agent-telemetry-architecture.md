---
doc: agent-telemetry-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Telemetry Architecture

> **Tier 3 architecture doc.** Read when touching `packages/agent/internal/observability/telemetry/`. Distinct from server-side OTel (`otel-pipeline-architecture.md`).

The agent's `telemetry` package is a thin wrapper around the shared OTEL `SwappableTracerProvider`. There is no operational-telemetry pipeline, no crash-report path, no anonymous-mode toggle, no PII redaction in this package today — those capabilities are roadmap items, not shipping behaviour.

---

## 1. Real implementation

```go
// packages/agent/internal/observability/telemetry/telemetry.go
//
// Package telemetry provides OTEL tracing for the Nexus Agent.
// It delegates to the shared SwappableTracerProvider.
package telemetry

import (
    "context"
    "log/slog"

    sharedtel "github.com/ai-nexus-platform/nexus-gateway/packages/shared/core/telemetry"
)

type Config   = sharedtel.Config
type Provider = sharedtel.SwappableTracerProvider

func Init(ctx context.Context, cfg Config, logger *slog.Logger) (*Provider, error) {
    return sharedtel.Init(ctx, cfg, logger)
}
```

That is the entire package: two type aliases and one `Init` thin-wrapper. All real behaviour (exporter wiring, sampling, hot-swap of the active provider) lives in `packages/shared/core/telemetry`.

Agent code spans (request lifecycle, hook execution, audit emit, etc.) flow through the OTEL provider returned by `telemetry.Init`. Service-health / operational signals the agent reports to Hub (heartbeats, queue depth, hook decisions, etc.) flow over the existing mTLS WS as `metrics_sample` events — that path is documented in `mq-architecture.md` §6 (the binding "metrics_sample WS is fine") and is **not** part of this package.

## 2. What's intentionally absent

The agent currently has **no operational-telemetry-class endpoint or crash-report pipeline beyond OTEL spans**. Specifically:

- No "operational telemetry" reporter for queue depth / cert validity / connection state — those values are surfaced over IPC to the agent UI (see `agent-tray-ipc-architecture.md`) and pushed to Hub as `metrics_sample` events, not via this package.
- No crash-report capture (no `event_type=agent.crash`), no opt-in toggle, no panic-handler that ships stack traces upstream.
- No PII redaction step in the agent telemetry path. File-path / IP anonymisation is **not** performed here; if a downstream consumer needs it, the OTEL exporter or the shared `core/telemetry` layer is the right place to add it.
- No "anonymous mode" admin toggle that strips `org_id` / `user_id` from outbound telemetry.

Adding any of these would be a new feature requiring its own SDD; do not "fix" their absence in a comment — the current shape is OTEL-only by design.

## 3. Sources

- `packages/agent/internal/observability/telemetry/telemetry.go` — the whole package (≈22 LOC).
- `packages/shared/core/telemetry/` — `SwappableTracerProvider`, `Init`, `Config`, exporter selection.
- `packages/agent/internal/observability/audit/` — separate audit/event pipeline (uses spans from this package but is a distinct system).

## 4. Cross-references

- `otel-pipeline-architecture.md` — server-side telemetry (uses the same shared provider).
- `audit-pipeline-architecture.md` — agent → Hub audit events (separate channel, not "telemetry" in the operational sense).
- `mq-architecture.md` §6 — `metrics_sample` WS exception used for fleet-management signals.
- `agent-tray-ipc-architecture.md` — UI surfaces health locally via IPC.
