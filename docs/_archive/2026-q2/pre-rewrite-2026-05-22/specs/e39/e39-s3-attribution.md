# E39-S3 — Traffic Attribution Pipeline

Status: draft
Epic: E39 — IAM & Identity Unification
Story: S3 — Traffic Attribution to User Identity

## User Story

As a compliance officer, I want all AI traffic — including traffic intercepted
by the compliance proxy — to be attributed to a specific user, so that there
is zero anonymous AI traffic in the enterprise.

---

## 1. Problem

`traffic_event` rows currently carry `entityId` and `entityType` for user
attribution, but the attribution pipeline has two gaps:

1. **Compliance-proxy events** — When the compliance proxy intercepts HTTPS
   traffic from a device, the Hub IdentityEnricher tries to resolve the source
   IP to a user. It currently reads `agent.Metadata["boundUserId"]`, which is
   a static enrollment-time binding and may not reflect the current logged-in
   user (or may be absent entirely if the device was not enrolled via the
   old flow).
2. **AI Guard calls** — The AI Guard endpoint (`/v1/guard`) on the AI Gateway
   accepts calls from the desktop agent. These calls are currently
   authenticated with a virtual key rather than a per-user JWT, so
   `traffic_event.userId` is the VK owner (an org-level identity), not the
   actual employee at the keyboard.
3. **thing_id / thing_name attribution** — Compliance-proxy events lack a
   `thing_id` linking them to the source device (ThingAgent), making it
   impossible to join traffic events with the device registry for policy
   evaluation or dashboard display.

---

## 2. Goal

- Add `thing_id` and `thing_name` columns to `traffic_event` so every event
  can be linked to a device.
- Upgrade the Hub IdentityEnricher to resolve device identity via
  `DeviceAssignment` (IP + time-window query) instead of the static metadata
  field.
- Authenticate AI Guard calls from the agent via a short-lived user JWT
  issued by the CP auth-server, replacing the VK-based authentication.

---

## 3. Non-goals

- Enriching existing historical `traffic_event` rows — new columns are
  nullable; backfill is not performed.
- Introducing any new traffic routing logic — this story is attribution-only.
- AI Guard policy evaluation changes — only the authentication mechanism
  changes; policy logic is untouched.
- Attributing traffic from non-agent HTTP clients that do not carry a JWT
  (those continue to be attributed via VK ownership).

---

## 4. Design

### 4.1 traffic_event Schema Additions

```sql
ALTER TABLE "traffic_event"
  ADD COLUMN "thing_id"   TEXT,
  ADD COLUMN "thing_name" TEXT;

CREATE INDEX "traffic_event_thing_id_timestamp_idx"
  ON "traffic_event" ("thing_id", "timestamp")
  WHERE "thing_id" IS NOT NULL;
```

Both columns are nullable. All existing INSERT paths must be updated to
include them (passing `NULL` when the device is not known — never omit the
column from the INSERT to avoid "column count mismatch" errors if the INSERT
uses positional parameters).

### 4.2 Compliance-Proxy Event Writer Update

**File**: `packages/compliance-proxy/internal/` (audit writer / event
emitter)

When writing a `traffic_event` for an intercepted request:

1. Look up the source device from the current ThingAgent registry
   (maintained by the proxy via Hub WebSocket push). Match on
   `ThingAgent.lastKnownIP` or an IP-indexed in-memory map.
2. If a matching `ThingAgent` is found:
   - Set `thing_id = thingAgent.ThingId`
   - Set `thing_name = thingAgent.Name`
3. Write `thing_id` and `thing_name` to the `traffic_event` INSERT.

Note: user-level attribution (`entity_id`, `entity_type = "user"`) is still
handled by the Hub IdentityEnricher asynchronously (§4.3). The proxy only
writes the device-level attribution it can resolve synchronously.

### 4.3 Hub IdentityEnricher: DeviceAssignment-Based IP Match

**File**: `packages/nexus-hub/internal/enricher/identity_enricher.go`
(or equivalent)

Current logic: `tryIPAgentMatch` looks up `agent.Metadata["boundUserId"]`
for the source IP.

New logic:

```sql
SELECT da.userId, da.deviceId, ta.name AS deviceName
FROM "DeviceAssignment" da
JOIN "ThingAgent" ta ON ta.id = da.deviceId
WHERE da.ipAddress = $sourceIP
  AND da.assignedAt <= $eventTimestamp
  AND (da.releasedAt IS NULL OR da.releasedAt > $eventTimestamp)
ORDER BY da.assignedAt DESC
LIMIT 1;
```

If a row is returned:
- Set `traffic_event.entityId = userId`
- Set `traffic_event.entityType = "user"`
- Set `traffic_event.thing_id = deviceId`
- Set `traffic_event.thing_name = deviceName`

Fall back to the previous `boundUserId` metadata lookup only if the
`DeviceAssignment` query returns no rows (backward compatibility for devices
not yet upgraded to E39-S2 login tracking).

The enricher continues to run asynchronously (post-INSERT update); the new
SQL query must complete within the existing enricher SLA
(< 100 ms p99 is acceptable for a simple indexed lookup).

### 4.4 AI Guard JWT Authentication

**Context**: The AI Guard endpoint (`/v1/guard`) on the AI Gateway is
invoked by the desktop agent to check whether a prompt is allowed before
sending it upstream. Currently authenticated with a VK.

**New flow**:

1. The CP auth-server exposes a short-lived JWT issuance endpoint for the
   agent (scope: `"ai-guard:invoke"`). The JWT includes:
   - `sub`: NexusUser.id
   - `device`: ThingAgent.id
   - `scope`: `"ai-guard:invoke"`
   - `exp`: now + 5 minutes (refreshed by the agent before expiry)
2. The AI Gateway `/v1/guard` handler validates the JWT:
   - Fetch the JWKS from CP: `GET /.well-known/jwks.json`
   - Verify signature, `exp`, and `scope` contains `"ai-guard:invoke"`.
   - Extract `sub` (userId) and `device` (thingId) claims.
3. Write to `traffic_event`:
   - `entity_id = sub`
   - `entity_type = "user"`
   - `thing_id = device`
   - `thing_name` = resolved from thingId (in-memory registry or DB lookup)

JWKS caching: the AI Gateway caches the JWKS for 5 minutes
(`sync.Map[kid → *rsa.PublicKey]`). On key rotation, a 401 response with
`WWW-Authenticate: Bearer error="invalid_token"` triggers a cache invalidation
and one retry.

VK-based authentication for `/v1/guard` is removed once the agent has been
updated to use JWTs. For the transition period, both paths are accepted:
if the `Authorization: Bearer` token passes VK lookup, use VK attribution;
if it passes JWT validation, use JWT attribution. The VK path is removed in
the next agent release cycle.

---

## 5. Tasks

- T1 — DB migration: add `thing_id TEXT` and `thing_name TEXT` columns to
  `traffic_event`; add index on `(thing_id, timestamp) WHERE thing_id IS NOT NULL`.
- T2 — Compliance-proxy event writer: populate `thing_id` and `thing_name`
  when the source IP matches a known `ThingAgent` in the local registry.
- T3 — Hub IdentityEnricher (`tryIPAgentMatch`): replace
  `agent.Metadata["boundUserId"]` lookup with the `DeviceAssignment` IP +
  time-window SQL query. Fall back to metadata lookup if no assignment found.
- T4 — AI Gateway: add JWT validation middleware for `/v1/guard`. Validate
  signature via CP JWKS (`/.well-known/jwks.json`), verify `scope` contains
  `"ai-guard:invoke"`, extract `sub`/`device` claims for `traffic_event`
  attribution. Cache JWKS for 5 minutes.

---

## 6. Acceptance Criteria

- AC1 — After migration, `traffic_event` has `thing_id` and `thing_name`
  columns; existing rows have `NULL` in both (no backfill).
- AC2 — For compliance-proxy-sourced events where the source device is
  identifiable, `thing_id` is populated at INSERT time (not deferred to
  enricher).
- AC3 — For compliance-proxy-sourced events, `entity_id` is populated
  within one enricher cycle after INSERT; the lookup uses `DeviceAssignment`
  IP + time-window matching.
- AC4 — AI Guard calls from the agent authenticated with a valid JWT result
  in `traffic_event.entity_type = "user"` and `traffic_event.entity_id =
  sub` from the JWT.
- AC5 — A device with an active `DeviceAssignment` at the time of a
  compliance-proxy event has both `thing_id` and `entity_id` populated on
  that event.
- AC6 — The IdentityEnricher falls back gracefully to metadata-based lookup
  when no `DeviceAssignment` row matches the source IP + timestamp.
- AC7 — JWKS fetch failure on the AI Gateway does not crash the process;
  the guard request returns `503` with a JSON error body.

---

## 7. Risks

- **R1** — The `DeviceAssignment.ipAddress` may be a private NAT address
  shared by multiple devices (e.g. all devices behind a corporate NAT exit
  the same IP). Mitigation: the time-window constraint `assignedAt <=
  event_ts AND releasedAt > event_ts` limits false matches; document this
  as a known limitation for NAT environments and recommend per-device IP
  assignment via MDM/DHCP reservation.
- **R2** — JWKS endpoint on CP may be slow or unavailable when the AI
  Gateway starts. Mitigation: JWKS is fetched lazily on first JWT-authenticated
  request; failures are logged and the request returns 503 (not a panic or
  startup failure).
- **R3** — Adding `thing_id` and `thing_name` columns to the `traffic_event`
  INSERT across multiple writers (AI Gateway, Compliance Proxy, Hub enricher
  update path) is a broad change. Mitigation: use named parameter SQL
  (`$thing_id`) rather than positional parameters; missing columns default to
  NULL without breaking existing INSERT statements.
- **R4** — The short-lived JWT (5 min TTL) requires the agent to implement a
  proactive refresh loop. If the agent is offline and the token expires, the
  next guard call will get a 401. Mitigation: the agent should refresh at
  TTL/2 (2.5 min); on 401, attempt one synchronous refresh before reporting
  the guard call as failed.
