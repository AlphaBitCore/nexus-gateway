# API Hub

*Audience: contributors and operators working with the Hub coordination layer; integrators building tooling that queries node status or manages jobs.*

Nexus Hub is the service registry and config-sync coordinator for the 5-service architecture. It exposes an HTTP API on `:3060` with two distinct route groups, each with its own auth credential. The `/api/hub/*` group is the internal control channel used by the Control Plane to push config updates, inspect node state, and manage scheduled jobs. The `/api/internal/things/*` group is the service-registration channel used by AI Gateway, Compliance Proxy, Agent, and Hub itself for WebSocket-fallback registration, heartbeat, config pull, shadow reporting, and audit batch upload. Neither route group is exposed to end-user applications or to the admin UI directly — the admin UI calls the Control Plane, and the CP calls Hub.

---

## Route groups and auth

| Route prefix | Caller | Auth credential | Purpose |
|---|---|---|---|
| `/api/hub/*` | Control Plane | `INTERNAL_SERVICE_TOKEN` (shared secret, `[MUST MATCH]` across CP and Hub) | Admin ops: push config, query nodes, manage jobs, generate enrollment tokens |
| `/api/internal/things/*` | AI Gateway, Compliance Proxy, Agent, Hub self | Per-service bearer token (service token for servers; device token for agents) | Service lifecycle: register, heartbeat, config pull, shadow report, audit upload |

The `INTERNAL_SERVICE_TOKEN` is a shared secret set via environment variable on both the Control Plane and Nexus Hub. It is never stored in YAML. Service tokens are provisioned during the Hub registration handshake and stored in the service's own config.

---

## Hub API — `/api/hub/*`

### Config update — `POST /api/hub/config/update`

The primary config-push endpoint. Called by the Control Plane after any admin CRUD operation that changes a service's configuration. Hub updates the desired state for all nodes of the given `thingType`, pushes a change signal over WebSocket to all online nodes, and writes a `config_change_event` row for the audit trail.

Request body:
```json
{
  "thingType": "compliance-proxy",
  "configKey": "hook_config",
  "state": { "version": 13 },
  "action": "update",
  "actorId": "user-admin-001",
  "actorName": "admin@nexus.ai",
  "sourceIp": "10.0.0.1"
}
```

Response includes `thingsNotified` (how many online nodes received the WebSocket push) and `thingsOnline` (total online for that type), which are reflected in the Config Sync UI.

### Node list and detail

- `GET /api/hub/things` — paginated list of all registered nodes, filterable by type, status, and name.
- `GET /api/hub/things/{id}` — full node detail including desired and reported config state.
- `GET /api/hub/things/{id}/shadow` — side-by-side desired vs reported per config key with per-key sync status.
- `GET /api/hub/drift` — list of nodes with `status=drift` or a version mismatch.

### Config operations

- `GET /api/hub/config/catalog` — list all `(thingType, configKey)` pairs present in the config templates. Used by the admin UI's Config Sync history filter.
- `GET /api/hub/config/history` — paginated config change event history, filterable by `thingType`, `configKey`, actor, and time range.
- `POST /api/hub/things/{id}/resync` — replay the current desired state for one config key to a specific node without bumping the template version or writing a new change event. Used by the "Re-sync this key" button on the Node Detail page.

### Scheduled jobs

Hub runs internal scheduled jobs (drift detector, metric rollup, job-run retention, etc.). The admin UI's Jobs page is backed by these endpoints:

- `GET /api/hub/jobs` — list all jobs with live status (last run, next run, error count).
- `GET /api/hub/jobs/{id}` — single job detail.
- `PUT /api/hub/jobs/{id}` — enable or disable a job (only the `enabled` field is mutable via API).
- `POST /api/hub/jobs/{id}/trigger` — manually trigger a job immediately.
- `GET /api/hub/jobs/{id}/runs` — paginated run history for a job.

### Agent enrollment tokens

- `POST /api/hub/enrollment/token` — generate an enrollment token for agent provisioning (with label, TTL, max-uses, and metadata).
- `GET /api/hub/enrollment/tokens` — list all enrollment tokens with usage counts.

---

## Internal Things API — `/api/internal/things/*`

This group is the HTTP fallback for service-to-Hub coordination. In normal operation, services use a WebSocket connection to Hub for real-time change signals. When the WebSocket is unavailable, these HTTP endpoints provide equivalent semantics with polling. Audit batch uploads always use HTTP regardless of WebSocket state because batch payloads may be large.

| Endpoint | Purpose |
|---|---|
| `POST /api/internal/things/register` | One-time (or crash-recovery) registration. Creates or updates the node row. Returns current desired config for startup recovery. |
| `POST /api/internal/things/heartbeat` | Periodic liveness check. If the node's `reportedVer` is behind the desired version, the response includes the new desired config. |
| `POST /api/internal/things/shadow` | Report the node's actual (applied) config state so Hub can compute the desired vs reported diff. |
| `GET /api/internal/things/config` | Bulk config pull — all config keys for the given `thingType`. When `?id=` is set, merges per-node overrides over the templates. |
| `GET /api/internal/things/config/{key}` | Single config key pull. |
| `POST /api/internal/things/audit` | Agent batch audit upload. Up to 1,000 events per batch. Events are written to the `nexus.event.agent` NATS subject for Hub consumers to process. |
| `POST /api/internal/things/deregister` | Mark the node offline on graceful shutdown. |

---

## WebSocket protocol

The WebSocket connection is the primary transport for Hub-to-node coordination. It carries three types of frames:

- **Config change signal** — Hub sends a `config_changed` event when a config key's desired state version advances. The node pulls the new config via `GET /api/internal/things/config/{key}` (pull-only model — Hub never pushes the full config state inline over WebSocket).
- **Heartbeat frames** — periodic liveness signals in both directions.
- **Metrics sample frames** — nodes send operational metric samples directly over the WebSocket to Hub without going through NATS.

The WebSocket endpoint is not documented in the OpenAPI spec because it is a binary framing layer, not an HTTP request-response surface. Protocol details are in [`ws/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/nexus-hub/internal/ws/) inside the Hub package.

---

## Canonical docs

- [`e3-hub-api.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/hub/e3-hub-api.yaml) — full Hub API OpenAPI 3.1 spec (both route groups)
- [`nexus-hub-internals-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md) — Hub internal subpackage reference
- [`thing-model.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-model.md) — node model, shadow contract, desired/reported semantics
- [`thing-config-sync-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md) — pull-only config sync mechanics

**Adjacent wiki pages**: [API-Overview](API-Overview) · [API-Authentication](API-Authentication) · [Hub-Coordination](Hub-Coordination) · [Thing-Model-And-Config-Sync](Thing-Model-And-Config-Sync) · [Control-Plane-Infrastructure-Pages](Control-Plane-Infrastructure-Pages)
