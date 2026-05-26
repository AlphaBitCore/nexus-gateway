# E22-S1 — Payload Capture End-to-End

Status: draft (awaiting design confirmation before code)
Epic: E22 — Compliance body storage (new)

## 1. Problem

The `traffic_event_payload` table, the `mq.TrafficEvent.RequestBody / ResponseBody`
fields, and the `compliance-proxy` `audit.Event.RequestBody / ResponseBody`
fields were all added in preparation for per-request body storage but were
never wired at runtime. grep across the repo finds zero writers of those
fields. Meanwhile:

- `compliance-proxy/internal/proxy/forward_handler.go` hardcodes
  `maxRequestBodySize = 10 * 1024 * 1024`.
- `ai-gateway/internal/handler/proxy.go` hardcodes the same.
- `thing_config_template.payload_capture` was seeded for CP and AG but had
  no reducer; P1 of E3-S5 dropped the seed rows on the ground that shipping
  a dead reducer is dishonest. Re-seeding is blocked on this feature.
- There is no admin API or UI for editing `payload_capture` config.
- There is no admin API or UI for editing `audit.payload` in
  `system_metadata`.

The upstream design doc at
`docs/_archive/2026-q2/brainstorms/2026-04-14-pii-redaction-and-payload-storage-design.md`
bundles body storage with PII redaction. This SDD carves the body-storage
slice out so it can land independently. PII redaction stays behind the
existing PiiDetector hook and is out of scope here.

## 2. Goal

Make per-request body capture a live, admin-controlled feature on both data
plane services. Specifically, after this lands:

1. Admin can edit a global `payload_capture` config from the Control Plane UI
   with three knobs: `store_request_body` (bool), `store_response_body`
   (bool), `max_body_bytes` (int, default 65536).
2. Saving the config:
   - Persists it to `system_metadata["payload_capture.config"]`.
   - Pushes a `payload_capture` shadow invalidation to `compliance-proxy`
     and `ai-gateway` via Hub.
3. Each service holds an atomic `PayloadCaptureConfig` snapshot in memory.
   The shadow reducer re-reads `system_metadata` on invalidation and swaps
   the atomic pointer. Out-of-band reloads on service start also populate
   the pointer from the same source so fresh boots are not stuck on zero
   values.
4. The body-reading path (forward_handler in CP, proxy in AG) reads the
   active `max_body_bytes` from the store instead of the const. Every
   `io.LimitReader` usage currently tied to `maxRequestBodySize` switches
   to the runtime value.
5. After the hook pipeline returns (and any body slice it mutated), the
   service populates `audit.Event.RequestBody` / `.ResponseBody` iff the
   corresponding store-flag is true and the body is non-empty. Downstream
   `mq.TrafficEvent` carries those fields; the Hub db-writer writes them
   to `traffic_event_payload`.

## 3. Non-goals

- PII redaction before storage — covered by the existing PiiDetector hook;
  if the body was mutated by a Modify hook, storage uses the mutated bytes.
- Per-nodeType or per-tenant overrides — a single global config for the
  whole fleet is v1.
- Field-level redaction of stored bodies (e.g. masking card numbers inside
  stored JSON) — out of scope, handled upstream by hooks.
- Streaming chunked body capture (`text/event-stream`) — v1 captures only
  the final, already-aggregated body slice that the proxy already has in
  memory for hooks. Streaming bodies are intentionally not stored.
- Retention / deletion policy of `traffic_event_payload` — separate feature
  (`docs/_archive/2026-q2/programs/handoff-2026-04-16.md` mentions a 30-day policy).

## 4. Design

### 4.1 Config shape

`system_metadata["payload_capture.config"]`:

```json
{
  "storeRequestBody":  false,
  "storeResponseBody": false,
  "maxBodyBytes":      65536,
  "maxRequestBytes":   10485760,
  "maxResponseBytes":  10485760
}
```

Defaults are conservative on the audit toggles (off) but deliberately
generous on the network caps: `maxRequestBytes` and `maxResponseBytes`
default to 10 MiB so the data plane never silently truncates client
payloads or upstream responses, while `maxBodyBytes` (64 KiB) bounds
only how much of each captured body is persisted to
`traffic_event_payload`.

The three caps are independent:

- `maxRequestBytes` — inbound request body read cap on the proxy
  handler. Bytes above this are rejected with `413 Payload Too Large`;
  the request never reaches the upstream.
- `maxResponseBytes` — upstream non-streaming response read cap on
  provider adapter `LimitedReadAll`. Overflow returns `502` with
  `upstream_error`. Streaming responses are unaffected (they are
  governed by the per-stream policies in `shared/streaming`).
- `maxBodyBytes` — audit-truncation cap. Applied only when stamping
  `audit.Event.RequestBody` / `.ResponseBody`; flips
  `request_truncated` / `response_truncated` when the live body is
  longer than this cap. Never affects forwarded bytes.

Conflating any two of these is the bug fixed in this iteration:
`maxBodyBytes` was previously read as the inbound request cap on the
AI Gateway, so any client (e.g. Claude Code) whose body exceeded
64 KiB had its payload silently sliced before being relayed upstream.

### 4.2 Storage model — `system_metadata`, NOT shadow template state

Follow the `observability` precedent:

- Admin UI writes to `system_metadata["payload_capture.config"]`.
- `InvalidateConfig("compliance-proxy", "payload_capture")` and
  `InvalidateConfig("ai-gateway", "payload_capture")` push a Cat B version
  bump.
- Each service's `OnConfigChanged` reducer for `payload_capture` re-reads
  `system_metadata` via its existing DB connection (CP has its own handle;
  AG has `db.ai_gateway` already set up for other Cat B keys).
- `thing_config_template` for `payload_capture` is a placeholder Cat B row
  `state: {}` for CP and AG, seeded so the initial shadow snapshot is not
  missing the key. Mirrors how `hook_config` / `routing_rules` seed as
  `{}` and the real data lives in their own tables.

### 4.3 Runtime consumer wiring

New package `shared/payloadcapture` (or split per service — see §5 open
question 1):

```go
type Config struct {
    StoreRequestBody  bool
    StoreResponseBody bool
    MaxBodyBytes      int64 // audit truncation cap; default 64 KiB
    MaxRequestBytes   int64 // inbound request body read cap; default 10 MiB
    MaxResponseBytes  int64 // upstream non-stream response read cap; default 10 MiB
}

type Store struct { cfg atomic.Pointer[Config] }

func NewStore(initial Config) *Store
func (s *Store) Get() Config
func (s *Store) Set(cfg Config)
```

Each service:

- Constructs the store at startup with values read from
  `system_metadata["payload_capture.config"]` (or defaults when missing).
- Wires the store into the intercept / proxy handler that owns
  `readBody`. The inbound read uses `store.Get().MaxRequestBytes`;
  overflow returns `413 PAYLOAD_TOO_LARGE` and the audit row is
  finalised before the handler returns.
- Provider adapters thread `store.Get().MaxResponseBytes` into the
  `LimitedReadAll` helper used for non-streaming upstream responses;
  overflow returns `502 upstream_error`.
- After the hook pipeline returns the (possibly modified) request body
  and before emitting the audit event, copies the body bytes truncated
  to `store.Get().MaxBodyBytes` into `audit.Event.RequestBody` iff
  `store.Get().StoreRequestBody`. The same truncation governs the
  response capture in the non-streaming response handler. The capture
  cap **never** affects what is forwarded; it only bounds what is
  persisted to `traffic_event_payload`.

### 4.4 Admin API

`/api/admin/settings/payload-capture`:

- `GET` → reads the `system_metadata` row, defaults to zero-value when
  absent.
- `PUT` → validates (`maxBodyBytes >= 0`, reasonable upper bound — 10 MB?),
  writes `system_metadata`, calls `InvalidateConfig` for both CP and AG,
  emits audit entry with `Action=update`, `EntityType=payloadCaptureConfig`.

IAM: `admin:ReadSettings` / `admin:UpdateSettings` (same as observability).

### 4.5 UI

`SettingsPayloadCaptureTab.tsx` alongside `SettingsObservabilityTab`.
Two Switches + three numeric Inputs (`maxBodyBytes` for audit
truncation, `maxRequestBytes` and `maxResponseBytes` for network read
caps). Saves via `systemApi.updatePayloadCaptureConfig` in
`packages/control-plane-ui/src/api/services/system.ts`.

i18n namespace `pages:settingsPayloadCapture` with en / zh / es entries
(copied to `public/locales/`). Key set: `title`, `subtitle`,
`storeRequest`, `storeResponse`, `maxBytes`, `maxBytesHelp`,
`maxRequestBytes`, `maxRequestBytesHelp`, `maxResponseBytes`,
`maxResponseBytesHelp`. The two network-cap helps must explicitly state
"affects forwarded bytes; oversized requests are rejected with 413 / 502"
so admins do not confuse them with the audit truncation cap above.

Routing: add a new tab trigger in `SettingsPage.tsx` between
Observability and wherever it fits product-wise.

### 4.6 Shadow reducer

`case "payload_capture":` in both `cmd/compliance-proxy/main.go` and
`cmd/ai-gateway/main.go` `OnConfigChanged`:

1. Reload config from `system_metadata["payload_capture.config"]`.
2. `payloadCaptureStore.Set(...)`.
3. `reported[key] = cs` — echo the inline state so the Hub diff stays
   synced at the per-key content level.

Errors are logged but do NOT fail the whole report batch (consistent with
observability).

### 4.7 Re-seeding `thing_config_template`

`tools/db-migrate/seed/seed.ts` re-adds two rows (deleted in E3-S5 P1):

```
{ type: 'compliance-proxy', config_key: 'payload_capture', state: {}, updated_by: 'seed-script' }
{ type: 'ai-gateway',       config_key: 'payload_capture', state: {}, updated_by: 'seed-script' }
```

`state: {}` (Cat B placeholder) — the real values live in
`system_metadata`. Matches how `hook_config` seeds as `{}` for the same
reason.

## 5. Open questions — please decide before code

### Q1 — Store package placement

Where should `payloadcapture.Store` live?

- A. `packages/shared/policy/payloadcapture/` — one package, used by both
     services. Matches how hooks / traffic / compliance live in shared.
- B. Per-service: `packages/compliance-proxy/internal/payloadcapture/` +
     `packages/ai-gateway/internal/payloadcapture/`. Clearer ownership but
     doubles the code.

Recommend A.

### Q2 — Body source for storage on CP

CP currently reads the body once for hooks, then forwards upstream. The
captured-for-audit copy should come from:

- A. The bytes passed INTO hooks (original client bytes, unmodified).
- B. The bytes AFTER hook modifications (if a hook ran `Modify` and
     returned new content — and on CP today `allowModify=false` so this
     never happens, but leaving the option clarifies the AG side).
- C. Both, stored separately (e.g. `request_body_original`,
     `request_body_after_hooks`). Doubles storage.

Recommend A for CP (`allowModify=false`), and A for AG too — "what the
caller sent" is the audit of record. Hook modifications are already audited
separately in hook execution logs.

### Q3 — Max body bytes ceiling

Admin-set `maxBodyBytes` should be capped at a server-side max (e.g. the
existing 10 MB const). What's the ceiling?

- A. 10 MB (current hardcoded value).
- B. 25 MB (cover vision models).
- C. Unbounded admin choice (risk).

Recommend A to start; revisit once someone asks for more.

### Q4 — Response body capture on streaming endpoints

Streaming SSE responses are NOT stored per §3. But the
`storeResponseBody=true` flag reads as "capture for every call" to a
non-technical admin. Options:

- A. Silently skip streaming responses (document only — UI doesn't
     surface the exception).
- B. UI label explicitly reads "Store response body (non-streaming only)".
- C. Add a separate `storeStreamingResponseBody` flag.

Recommend B.

### Q5 — Admin write path auth + audit linkage

CP's `admin_extras.go:UpdateObservability` already enforces
`admin:UpdateSettings` IAM and emits an audit entry. Same pattern here?

- A. Yes, mirror observability verbatim.
- B. Also add a "PII-sensitive setting" extra confirmation dialog in UI
     before saving (since enabling body storage is a compliance event).

Recommend A + B — minimal server change, UI adds a confirmation modal.

## 6. Task list

Assuming Q1=A, Q2=A, Q3=A, Q4=B, Q5=A+B:

1. **schema** — none.
2. **shared/payloadcapture** package — `Config`, `Store`, default value
   constants, unit tests.
3. **compliance-proxy**:
   - Wire store into `cmd/compliance-proxy/main.go`; initial read from
     `system_metadata`.
   - `forward_handler.readBody` reads `store.Get().MaxBodyBytes`.
   - After hook pipeline, copy bytes into `audit.Event.RequestBody` iff
     flag set. Same for response body in the response-capture call site.
   - `OnConfigChanged` `case "payload_capture"` that re-reads
     system_metadata and calls `store.Set(...)`.
   - Delete `maxRequestBodySize` const.
4. **ai-gateway**: same plan mirrored in `cmd/ai-gateway/main.go` + the
   proxy handler.
5. **control-plane admin handler**
   (`packages/control-plane/internal/handler/admin_extras.go`):
   `GetPayloadCaptureConfig`, `UpdatePayloadCaptureConfig`. Register
   `GET|PUT /api/admin/settings/payload-capture` in
   `admin_routes.go`.
6. **OpenAPI** — extend the admin OpenAPI spec with the new settings
   endpoint.
7. **control-plane-ui**:
   - `services/system.ts`: add `getPayloadCaptureConfig` /
     `updatePayloadCaptureConfig` + `PayloadCaptureConfig` type.
   - `SettingsPayloadCaptureTab.tsx` with the three controls and the
     confirmation dialog (Q5-B).
   - Wire tab into `SettingsPage.tsx`.
   - i18n keys in en/zh/es both src and public locales.
8. **seed** — re-add the two `payload_capture` rows to
   `thing_config_template`.
9. **docs** — update `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md §4.5` to list
   `payload_capture` under CP and AG again; update
   `docs/developers/specs/e3/e3-s5-config-sync-remediation.md` §4 P1 note that
   payload_capture has been reintroduced (pointer to this SDD).
10. **tests**:
    - unit: shared/payloadcapture Store swap, Get default when nil,
      reducer parsing
    - integration-ish: the CP forward_handler reads the dynamic value,
      not the const; changing it at runtime affects the next request's
      LimitReader — mock via `httptest`
    - UI: vitest for the Settings tab covering save + confirm dialog
11. **verify**: `go build` + `go test -race -count=1` all touched
    services; `npx tsc` + `npx vitest run` for UI.

## 7. Acceptance criteria

1. Admin Settings → Payload Capture shows the current config, defaults to
   all-off at 64 KiB.
2. Toggling `storeRequestBody` on and hitting Save triggers a visible
   confirmation; on confirm, returns 200.
3. The next request through CP or AG is captured: querying
   `SELECT * FROM traffic_event_payload` has a fresh row within 10 s.
4. Lowering `maxBodyBytes` to 4096 causes large requests to be truncated
   at the exact configured value (verified via test payload).
5. Each service's per-key shadow diff on the agent / admin Infrastructure
   page shows `payload_capture` as in-sync.
6. No regressions: hook pipeline and normal forwarding still work with
   feature toggled off (default state).

## 8. Risks

- **Large body storage** blowing up `traffic_event_payload` table — call
  out in admin UI; separate retention job (out of scope) is needed soon.
- **Data sensitivity** — admin must be aware that enabling body storage
  means request bodies (potentially containing user prompts) hit Postgres.
  Q5-B confirmation dialog covers the UX.
- **Race during reducer swap** — atomic pointer handles this; no in-flight
  request can see a partially-updated config.
- **Hook-modified bytes in AG** — when a hook returns `Modify`, AG already
  swaps the live body slice. Storing "what the caller sent" (Q2-A) means
  the capture path must copy bytes BEFORE hooks run, which costs one extra
  alloc per captured request. Measure; likely negligible at 64 KiB cap.

## 9. Follow-ups

- Retention policy for `traffic_event_payload` (30-day delete job).
- Field-level redaction integration if/when PiiDetector gains the
  `Modify` decision path.
- Agent body capture — **landed in E24-S1** (commits `07844373` C1 and
  C2 follow-up). Agent's intercept path now reuses the same
  `payloadcapture.Store`, receives the config via a new
  `AgentPayloadCaptureLoader` Cat B key, and stamps captured
  request/response bytes onto `traffic_event_payload` through the Hub
  audit endpoint. Admin toggle and server-side ceiling are shared with
  CP and AG via `system_metadata["payload_capture.config"]` — one row
  now drives all three data-plane services.
