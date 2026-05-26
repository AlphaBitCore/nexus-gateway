# Control Plane Infrastructure Pages

*Audience: platform operators running day-2 operations on the Nexus fleet.*

The Infrastructure section of the Control Plane UI is the operational console for the platform itself — it surfaces the health, configuration state, scheduled jobs, and emergency controls for every service and agent registered with Nexus Hub. Four sub-sections cover the most common ops workflows: Nodes, Config Sync, Jobs, and Kill Switch.

---

## Nodes

**Path**: `/infrastructure/nodes` — IAM action: `admin:node.read`

The Nodes page lists every registered service and agent (Hub, Control Plane, AI Gateway, Compliance Proxy, and all Desktop Agent instances). Each row shows:

- **Status** — online / offline / degraded.
- **Version** — binary version as reported by the service on last heartbeat.
- **Drift indicator** — surfaces when the service's applied config diverges from the target config for more than the allowed window.

From this page, operators can trigger a force-resync on a node (`admin:node.force-resync`) to push the current target config to a stalled service. The underlying API endpoint is `POST /api/admin/nodes/:id/force-resync`.

All registered services and agents register with Nexus Hub as Things. The Nodes page reflects the Hub's Thing registry — if Hub is down, the page shows only what was last cached.

## Config Sync

**Path**: `/infrastructure/config-sync` — IAM action: `admin:settings.read`

Config Sync surfaces the per-service configuration state in a human-readable form. For each service, the page shows:

- **Target config** (desired) — what the admin has set via the admin API.
- **Applied config** (reported) — what the service is actually running.
- **Per-key diff** — which specific config keys differ.
- **`applyError`** — the error message if the service failed to apply a key.
- **`processStartedAt`** — when the service last started its config-apply cycle.

The underlying config sync model is Hub-centric and pull-only: Hub holds the authoritative shadow blob; services pull their config on boot and on each Hub-pushed change signal. Config Sync visualises this shadow state.

A small amount of transient drift is normal — services in the middle of applying new config show a diff briefly. Persistent drift for more than a few minutes indicates an apply failure; look at `applyError` for the root cause.

## Jobs

**Path**: `/infrastructure/jobs` — IAM action: `admin:settings.read`

The Jobs page reflects the Hub's scheduled job catalogue. Hub is the single owner of the scheduler; all scheduled jobs run inside Hub as code. The page shows:

- **Job name** — the canonical `JobID` constant.
- **Last run** — timestamp and status (`success | failure | skipped`).
- **Duration** — histogram of recent runs.
- **Next scheduled** — derived from the cron spec.

Operators can trigger a job manually from this page (where exposed). The underlying API is `POST /api/admin/jobs/:name/trigger`.

Common job categories visible in the UI:

| Category | Examples |
|---|---|
| Config sync / drift | `config-drift-check`, `stale-thing-sweep` |
| Audit pipeline | `audit-chain-verify`, `audit-freshness-check` |
| Credentials | `credential-health-rollup`, `credential-reliability-alerts` |
| Provider health | `provider-health-rollup`, `provider-unavailable-alerts` |
| Metrics rollup | `rollup-5m`, `merge-1h`, `merge-1d` |
| Retention | `data-retention`, `job-retention` |
| Passthrough expiry | `passthrough.expiry` (auto-reverts emergency passthrough) |

Job metrics (`nexus_job_runs_total`, `nexus_job_duration_seconds`, `nexus_job_status_total`) feed the alerting pipeline — stalled or failing jobs trigger alert rules.

## Kill Switch

**Path**: `/infrastructure/kill-switch` — IAM action: `admin:kill-switch.read`

The Kill Switch page is the operator-facing surface for the emergency passthrough system. It provides the same activation capability as the AI Gateway → Passthrough page, scoped for operators who need quick access during incidents.

### Three tiers

| Tier | Scope | Effect |
|---|---|---|
| Global | All traffic | All requests bypass the selected enforcement flags |
| Adapter | Traffic matching one adapter type (e.g., `openai`) | Only that adapter's traffic bypasses |
| Provider | Traffic targeting one provider credential | Only that provider's traffic bypasses |

Tiers compose from broadest to most specific. If global is active, adapter and provider rows are ignored for the matching traffic.

### Bypass flags

Each activation specifies which enforcement to bypass:

- `bypassHooks` — skip the 3-stage hook pipeline.
- `bypassCache` — skip response cache lookup and population.
- `bypassNormalize` — skip canonical normalization (rare; for protocol emergencies).

### Activation requirements

- IAM action `admin:passthrough.emergency-enable` is required to flip a tier to active.
- A reason (free text) is required and is stamped on every `traffic_event` row during the bypass window as `bypass_reason`.
- Duration is capped at 8 hours (default 1 hour).

### Propagation timing

Activation reaches all affected services in under 5 seconds via Hub shadow push. The auto-revert job (`passthrough.expiry`) checks every ~60 seconds; expiry revert happens within 60 seconds of the configured expiry time.

### History

The Kill Switch History tab queries `AdminAuditLog` filtered to `resource = passthrough`. All activations, edits, and reverts are recorded there (performed by a human or the auto-revert job). The three tier tables in Postgres hold only the current state, not a history table.

---

## Canonical docs

- [`infrastructure.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/infrastructure.md) — full Infrastructure section page list, workflows, API endpoints
- [`jobs-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md) — scheduler model, job catalogue, passthrough auto-revert
- [`kill-switch-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md) — activation lifecycle, tier overlap, history model, IAM actions

**Adjacent wiki pages**: [Control Plane Admin UI Tour](Control-Plane-Admin-UI-Tour) · [Control Plane Overview](Control-Plane-Overview) · [Emergency Passthrough](Emergency-Passthrough) · [Thing Model And Config Sync](Thing-Model-And-Config-Sync) · [Kill Switch](Kill-Switch)
