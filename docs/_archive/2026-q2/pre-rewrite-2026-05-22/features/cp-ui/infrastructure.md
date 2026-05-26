# Infrastructure section — CP-UI feature doc

> Audience: platform operators. This section is the operational console for the platform itself — Things, config sync, jobs, killswitch, observability, build & rollout.

## Pages in this section

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Nodes | `/infrastructure/nodes` | `admin:node.read` | Hub + CP + AI GW + Compliance Proxy + agents (all Things), status, version |
| Config Sync | `/infrastructure/config-sync` | `admin:settings.read` | Per-Thing shadow desired vs reported, drift surface |
| Overrides | `/infrastructure/overrides` | `admin:node.write-override` | Per-Thing config overrides |
| Jobs | `/infrastructure/jobs` | `admin:settings.read` | Scheduled job catalogue, last-run, duration |
| Kill Switch | `/infrastructure/kill-switch` | `admin:kill-switch.read` | Three-tier emergency kill switch (org / provider / route) + auto-revert status |
| Agent Setup | `/infrastructure/agent-setup` | `admin:settings.read` | Self-service agent installer downloads per platform |
| Diagnostic Mode | `/infrastructure/diag-mode` | `admin:diagnostic-mode.read` | Enable per-Thing diag mode for verbose telemetry |
| Crashes | `/infrastructure/crashes` | `admin:settings.read` | Recent panics / fatal logs across services |
| Errors | `/infrastructure/errors` | `admin:settings.read` | Recent non-fatal error log aggregates |
| Observability Config | `/infrastructure/observability-config` | `admin:observability.read` | Metrics / tracing config; Prometheus targets |
| Observability Retention | `/infrastructure/observability-retention` | `admin:observability.read` | Retention policies for ops_metrics + audit tables |
| SIEM | `/infrastructure/siem` | `admin:audit-log.read` | SIEM bridge channels + recent forwards |
| Proxy Rollout | `/infrastructure/proxy-rollout` | `admin:settings.read` | Compliance-proxy / agent staged rollout rings |

## Common workflows

- **Spot-check a Thing's health** — Nodes → status column. Drift indicator surfaces if desired/reported diverge persistently.
- **Investigate a drift** — Config Sync → select Thing → see per-key desired vs reported + per-key applyError + processStartedAt.
- **Activate emergency passthrough** — Kill Switch → choose tier (org / provider / route) → reason + duration (max 8h, default 1h) → confirm. Hub propagates and reconciles every 60s (cross-ref `jobs-architecture.md` `kill_switch.reconcile`).
- **Trigger agent rollout** — Proxy Rollout → set rollout rings → start → progress per ring. Cancel if anomalies.
- **Tune retention** — Observability Retention → adjust per-table TTL → save. The retention purge job picks up the change on next tick.
- **Diagnose a crash** — Crashes → group by service + recent → view stack + correlated request_id (if any) → drill to audit row.

## Key API endpoints

```
/api/admin/nodes                       [GET]; GET /:id; POST /:id/reboot (rare)
/api/admin/config-sync                 [GET]; GET /:thingId
/api/admin/overrides                   [GET/POST/PUT/DELETE]
/api/admin/jobs                        [GET]; POST /:name/trigger
/api/admin/kill-switch                 [GET/POST]; POST /reset; GET /history
/api/admin/diagnostic-mode             [GET/PUT]
/api/admin/crashes                     [GET]
/api/admin/errors                      [GET]
/api/admin/observability/config        [GET/PUT]
/api/admin/observability/retention     [GET/PUT]
/api/admin/siem                        [GET/POST/PUT/DELETE]
/api/admin/proxy-rollout               [GET/POST]; POST /pause; POST /cancel
```

## Failure modes & gotchas

- **`config-sync` drift may be transient** — Things in-flight applying new config show drift briefly; alert thresholds tuned to ignore short windows.
- **Agent Setup PKG distribution** — manual scp + nginx `/downloads/`; no auto-updater wired (memory `reference_agent_pkg_distribution`). Canonical URL: `https://nexus.example.com/downloads/NexusAgent-latest.pkg`.
- **Kill Switch reconcile timing** — auto-revert happens within 60s of expiry, not at the exact second. UI clarifies "expiring around HH:MM".
- **Observability retention is destructive** — shortening retention will delete rows on next purge run; admin guard requires "I understand" confirmation.
- **SIEM channel test** — emits a synthetic event with `_test: true`; receivers should filter.

## Architecture references

- `docs/developers/architecture/cross-cutting/foundation/thing-model.md` — all Things visible here
- `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` — Config Sync mechanics
- `docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md` — Jobs page reflects this catalogue
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §4 — Kill Switch flow
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — Observability Retention scope
- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — Agent Setup ties to enrollment tokens
