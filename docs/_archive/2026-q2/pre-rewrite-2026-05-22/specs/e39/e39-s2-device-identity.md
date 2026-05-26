# E39-S2 — Device Identity & Trust

Status: draft
Epic: E39 — IAM & Identity Unification
Story: S2 — Device Identity & Trust Level

## User Story

As a security admin, I want to know which user is currently logged into each
device and what the device's trust level is, so that I can enforce device-
based access policies.

---

## 1. Problem

The current Agent enrollment model links a device to a fixed `boundUserId` in
`agent.Metadata`. This has two limitations:

1. **Static binding** — the user-device association is set at enrollment time
   and never changes, even if a different employee logs in to the same machine.
2. **No trust level** — the Control Plane and Hub have no computed signal that
   tells them how "trusted" a device is right now. There is no way to
   distinguish an enrolled device that is online and actively used by a known
   employee from one that is online but sitting at a login screen.

As a result:

- The traffic attribution enricher falls back to the stale `boundUserId` from
  metadata even when a different user is active.
- The Nodes page cannot show "who is on this machine right now".
- Access policies cannot reference a device trust level.

---

## 2. Goal

Introduce a lightweight **DeviceAssignment** model that records the currently
active user session on a device, and a **trust_level** integer on
`ThingAgent` that the Hub computes on every heartbeat.

- On agent login (token issuance by CP auth-server): create a
  `DeviceAssignment` row, close the previous one if different user.
- On heartbeat: Hub recomputes `trust_level` (0–3) and writes it to
  `ThingAgent` and the reported shadow.
- On agent logout (token revocation): close the active `DeviceAssignment`,
  downgrade trust to 1.
- The traffic attribution enricher (E39-S3) will consume the `DeviceAssignment`
  table using an IP + time-window query.

---

## 3. Non-goals

- Policy enforcement based on trust level (separate epic; this story only
  computes and surfaces the level).
- Multi-user session tracking (only the most recent active assignment per
  device is tracked).
- Windows / Linux login events sourced from OS event logs (only CP-issued
  agent tokens are tracked in this story).
- Certificate rotation or CA chain changes affecting trust computation
  (handled by the Hub PKI subsystem, not this story).

---

## 4. Design

### 4.1 Schema Changes

#### 4.1.1 DeviceAssignment model additions

`DeviceAssignment` already exists in the schema. Add the following fields:

```prisma
model DeviceAssignment {
  // existing fields …
  loginMethod  String?   // idp, local, sso, etc. — from auth token issuance
  tokenJti     String?   // JWT jti claim of the session token; used for revocation lookup
  ipAddress    String?   // IP address of the agent at login time (from CP auth request)
  // Make userId nullable to support pre-login "device online" assignments
  userId       String?   // was: String (required) — relaxed to optional
  // existing fields …
}
```

Add the `"login"` value to the `DeviceAssignmentSource` enum (if enum-typed)
or document it as an allowed string value if the column is plain `TEXT`.

DB migration:

```sql
ALTER TABLE "DeviceAssignment"
  ADD COLUMN "loginMethod" TEXT,
  ADD COLUMN "tokenJti"    TEXT,
  ADD COLUMN "ipAddress"   TEXT,
  ALTER COLUMN "userId"    DROP NOT NULL;
```

#### 4.1.2 Partial unique index on DeviceAssignment

Enforce the one-active-assignment-per-device constraint at the DB level:

```sql
CREATE UNIQUE INDEX "DeviceAssignment_deviceId_active_key"
  ON "DeviceAssignment" ("deviceId")
  WHERE "releasedAt" IS NULL;
```

This ensures at most one row per `deviceId` can have `releasedAt IS NULL` at
any time. The index is partial so closed (historical) assignments are not
affected.

#### 4.1.3 ThingAgent additions

```prisma
model ThingAgent {
  // existing fields …
  trustLevel           Int     @default(0)  // 0=Untrusted, 1=Enrolled, 2=Identified, 3=Compliant
  currentAssignmentId  String?              // FK to active DeviceAssignment.id (nullable)
  // …
}
```

DB migration:

```sql
ALTER TABLE "ThingAgent"
  ADD COLUMN "trustLevel"          INT NOT NULL DEFAULT 0,
  ADD COLUMN "currentAssignmentId" TEXT;
```

### 4.2 CP Auth-Server: Assignment Creation on Login

**File**: `packages/control-plane/internal/authserver/`
(exact file TBD during implementation; likely `token_issuance.go` or
`agent_token.go`)

On every successful agent token issuance:

1. Close any existing active `DeviceAssignment` for `deviceId` where
   `releasedAt IS NULL` by setting `releasedAt = now()`.
   - If the existing assignment has the same `userId`, this is a re-login;
     close and reopen.
   - If different `userId`, this is a user switch; close old, open new.
2. Insert a new `DeviceAssignment`:
   ```go
   DeviceAssignment{
     DeviceId:    deviceId,
     UserId:      &userId,          // from authenticated identity
     Source:      "login",
     LoginMethod: loginMethod,      // e.g. "oidc", "local", "sso"
     TokenJti:    jtiFromToken,
     IpAddress:   remoteIP,         // from http.Request.RemoteAddr (after proxy stripping)
     AssignedAt:  time.Now(),
     ReleasedAt:  nil,
   }
   ```
3. Update `ThingAgent.currentAssignmentId = newAssignment.id`.

Step 1 and 2 must execute in a single DB transaction to prevent a race
between concurrent logins on the same device.

### 4.3 Hub: Trust Level Computation on Heartbeat

**File**: `packages/nexus-hub/internal/` (heartbeat handler or thing-agent
update path)

On every agent heartbeat, compute `trust_level` as follows:

| Level | Name | Condition |
|---|---|---|
| 0 | Untrusted | Agent certificate is invalid or expired |
| 1 | Enrolled | Certificate valid; agent online; no active DeviceAssignment |
| 2 | Identified | Certificate valid; agent online; active DeviceAssignment exists |
| 3 | Compliant | Level 2 conditions met AND agent version ≥ minimum required version |

Pseudocode:

```go
func computeTrustLevel(agent *ThingAgent, assignment *DeviceAssignment, minVersion string) int {
    if !agent.CertValid {
        return 0
    }
    if assignment == nil {
        return 1
    }
    if semver.Compare(agent.AgentVersion, minVersion) >= 0 {
        return 3
    }
    return 2
}
```

The minimum required version is read from `system_metadata` (key:
`agent.min_trust_version`; default: `"0.0.0"` so level 3 is achievable by
any enrolled agent with an active assignment).

After computing the level:

1. Write `ThingAgent.trustLevel = level` to the DB.
2. Write the `trust_level` field into the `reported` section of the Hub
   device shadow for this `thingId`.

### 4.4 Agent Logout: Assignment Release

On agent token revocation (logout endpoint or token expiry sweep):

1. Look up `DeviceAssignment` by `tokenJti = revokedJti` AND
   `releasedAt IS NULL`.
2. Set `releasedAt = now()`.
3. Set `ThingAgent.currentAssignmentId = NULL`.
4. On the next heartbeat, the Hub will recompute `trustLevel = 1`
   (online, no assignment). The CP can also proactively write
   `ThingAgent.trustLevel = 1` immediately after step 3.

---

## 5. Tasks

- T1 — DB migration: add `loginMethod`, `tokenJti`, `ipAddress` columns to
  `DeviceAssignment`; make `userId` nullable.
- T2 — DB migration: add `"login"` as valid value for
  `DeviceAssignment.source` (document in Prisma schema comment or enum).
- T3 — DB migration: create partial unique index
  `DeviceAssignment_deviceId_active_key` on `(deviceId) WHERE releasedAt IS NULL`.
- T4 — DB migration: add `trustLevel INT DEFAULT 0` and
  `currentAssignmentId TEXT` to `ThingAgent`.
- T5 — CP auth-server: on agent token issuance, close existing active
  assignment (same device) and insert new `DeviceAssignment` row with
  `source="login"`, `loginMethod`, `tokenJti`, `ipAddress`. Update
  `ThingAgent.currentAssignmentId`. Wrap in a DB transaction.
- T6 — Hub heartbeat handler: compute `trust_level` (0–3) using cert
  validity, active assignment presence, and agent version vs.
  `agent.min_trust_version` from `system_metadata`. Write to
  `ThingAgent.trustLevel` and reported shadow field.
- T7 — CP auth-server: on token revocation, set `DeviceAssignment.releasedAt
  = now()` by `tokenJti` lookup, clear `ThingAgent.currentAssignmentId`,
  downgrade `ThingAgent.trustLevel` to 1 (or defer to next heartbeat).

---

## 6. Acceptance Criteria

- AC1 — At most one `DeviceAssignment` row per `deviceId` has
  `releasedAt IS NULL` at any time; the DB unique partial index enforces
  this.
- AC2 — When an agent user logs in (token issued), a new `DeviceAssignment`
  row is created with `source = 'login'`, and any previous active assignment
  for the same device is closed.
- AC3 — `ThingAgent.trustLevel` is updated within one heartbeat cycle of a
  login or logout event.
- AC4 — A device with a valid certificate, an active assignment, and an
  agent version above the configured minimum reports `trustLevel = 3`.
- AC5 — After logout (token revocation), the device's active assignment is
  closed and `ThingAgent.trustLevel` drops to 1 (or 0 if cert expires).
- AC6 — The CP UI Nodes page can display the current logged-in user
  (`DeviceAssignment → NexusUser.displayName`) and trust level badge for
  each agent device (UI wiring covered in S4).

---

## 7. Risks

- **R1** — The partial unique index may conflict with existing duplicate
  active assignments in development data. Mitigation: run a cleanup query
  before creating the index: close all but the most recent active assignment
  per device.
- **R2** — Agent IP address extraction from `http.Request.RemoteAddr` may
  return a proxy IP in environments with a load balancer. Mitigation: check
  `X-Forwarded-For` header first (only if CP is deployed behind a trusted
  proxy). Document the extraction logic with a comment.
- **R3** — The `agent.min_trust_version` `system_metadata` key may be absent
  from the DB in existing installations. Mitigation: default to `"0.0.0"` in
  the Hub reader so all online + assigned agents reach trust level 3.
- **R4** — Concurrent logins (two devices for the same user, or rapid
  re-login) may hit a race on the unique index. Mitigation: the transactional
  close-then-insert in T5 serialises per-device; concurrent logins on the
  same device are uncommon and will simply retry.
