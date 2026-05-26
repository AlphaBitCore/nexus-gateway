# Setup / Status / System sections — CP-UI feature doc

> Audience: admins for first-run bootstrap and ongoing system-level admin. Three small sidebar sections grouped for brevity.

## Setup

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Setup Wizard | `/setup` | `admin:settings.update` | Guided first-run: configure providers, enroll initial agents, smoke-test |

The wizard is shown on bootstrap when the system has no providers / VKs / agents configured. After completion, the wizard surface is hidden but the route remains reachable for re-running specific steps.

## Status

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Status Health | `/status` | `admin:status.read` | Cross-service health: Hub, CP, AI GW, Compliance Proxy, MQ, Postgres, Redis, S3 spillstore |

The status page calls `GET /api/admin/status/health` which aggregates `/health` from each Thing and dependencies. Useful when on-call needs a single screen before drilling.

## System

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| AI Gateway Simulator | `/tools/ai-gateway-simulator` | (varies) | Dev/test tool: craft a `/v1/*` request, see routing decision + which credential would be used + simulated cost, without actually invoking upstream |

## Common workflows

- **First-time install** — Setup → wizard step 1: add a provider → step 2: add a credential → step 3: test connection → step 4: issue a VK → step 5: optional agent enrollment → step 6: smoke test using AI Gateway Simulator.
- **On-call health check** — Status → expand any red row → links to detailed Infrastructure page for the affected Thing.
- **Pre-flight a routing rule** — AI Gateway Simulator → paste a candidate `/v1/chat/completions` payload → "Simulate" → see which rule matched, which credential would be picked, simulated cost.

## Failure modes & gotchas

- **Wizard partial completion** — wizard saves progress per step; an admin can leave and resume.
- **Simulator without real call** — the simulator does NOT call the provider. Cost is best-effort from pricing tables.
- **Status panel auto-refresh** — the Overview tab polls the ops-metrics endpoint every 15 s while visible (`packages/control-plane-ui/src/pages/status/overview/StatusPage.tsx:94-99`); the underlying `/ready` handler is live (no server-side TTL), so a Force-refresh button just re-fetches.

## Architecture references

- `docs/users/product/architecture.md` — top-level system overview (audience-friendly index)
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` — first-run flow lives in §5 (Agent enrollment + SSO)
- `docs/developers/architecture/services/ai-gateway/routing-architecture.md` — Simulator depends on this
- `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` — Simulator's credential-pick simulation
