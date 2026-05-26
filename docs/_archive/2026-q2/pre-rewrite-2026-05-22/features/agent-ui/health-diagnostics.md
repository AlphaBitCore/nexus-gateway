# Health & Diagnostics page — Agent UI feature doc

## Purpose

Self-service diagnostics for "is the agent working correctly on this device?". Used by both end-users and IT support on a call.

## Sections

### Connection

- Status (Connected / Reconnecting / Offline).
- Last successful Hub contact: `HH:MM:SS`.
- Current transport (WS / HTTP fallback).
- Round-trip latency to Hub (last sample).

### Certificate

- Subject + Issuer.
- Valid from / to.
- Days remaining.
- Last successful renewal (when Renew has been invoked); a "renew" button drives `Manager.Renew` against Hub — automatic scheduling is not wired today.
- Cross-link: "How does enrollment work?" → opens local doc.

### Audit Queue

- Queue depth (events not yet drained).
- Last drain at: `HH:MM:SS`.
- Drain rate (events/min).
- Spilled to S3 count (bodies overflow).

### Platform Intercept

- Platform: macOS (NE) / Linux (pf / iptables) / Windows (WinDivert).
- Interception status (Active / Disabled / Failed-open).
- Recent intercept errors (last 24h, capped).

### Resources

- Memory, CPU usage by the daemon + extension/driver.

### Self-test

Button: "Run self-test" → triggers a synthetic request through the local pipeline + audit upload → reports pass/fail with phase breakdown.

### Generate Support Bundle

Button: "Generate support bundle" → produces a `.tar.gz` with logs, current config (redacted), recent events (redacted), platform info. Stored locally; user shares with support.

## Data sources

All reads flow over the daemon statusapi (Unix socket / named pipe, `agent-tray-ipc-architecture.md`). The Wails UI calls into `AgentBridge` which sends single-line commands (e.g., `GET_STATUS`) and reads JSON back. The Self-test and Support-bundle buttons drive equivalent IPC commands; no HTTP `/local/...` API is exposed.

## Terminology

- "Audit queue" (not "event queue" — match the platform's term).
- "Platform intercept" not "NE provider" / "kernel filter" (those are too implementation-specific for this audience).

## Failure modes

- **NE Provider unhealthy** — surface in red. NEVER auto-restart from the UI; macOS NE is safety-critical (cross-ref `agent-ne-fail-open-architecture.md`). Surface support steps instead.
- **Cert renewal failed / expired** — surface in red + actionable next step ("Re-enroll with new token from admin"). Cross-ref `agent-enrollment-architecture.md` §6.
- **Audit queue persistent backlog** — surface in yellow; could indicate Hub unreachable or local rate limit.
- **Self-test fails** — show the failing phase + error class; cross-link to support.

## Architecture references

- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — Platform Intercept safety
- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — Cert lifecycle
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — Audit queue mechanics
- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — Self-test phase model
