# E37 — Payload Capture Unification

## Background

Payload Capture (originally shipped under E22) lets admins persist captured
request and response bodies onto `traffic_event_payload` so the Control Plane
UI can render them. The end-to-end pipeline works, but a code review on
2026-05-06 surfaced four architectural deviations from the intended design:

1. **Two thresholds where the design called for one.** `MaxBodyBytes`
   (admin-tunable, runtime) clipped the captured byte slice; a separate
   YAML field `spill.inlineThresholdBytes` (~256 KiB) decided inline-vs-spill
   independently. Admins editing the runtime knob could not influence where
   spill kicked in; admins editing the YAML had to push to every host.
2. **Hub writes spill files for the Agent's HTTP audit upload path.** The
   intent was that data-plane services (AI-Gateway, Compliance-Proxy, Agent)
   own the spill decision and the upload, with Hub acting only as a passive
   metadata sink. `nexus-hub/internal/handler/agent_audit.go` violated this
   by calling `spillstore.EmitBody` itself when the agent's HTTP envelope
   carried inline bytes.
3. **`MaxBodyBytes <= 0` semantic was inconsistent.** The new comment said
   "0 = unlimited"; three downstream call sites (`proxy_cache.go:533`,
   `agent/internal/intercept/payload_capture.go`, `cappedTeeWriter`)
   silently coerced to a 64 KiB cap or captured zero bytes.
4. **Coverage gaps.** Compliance-Proxy never applied any cap. AI-Gateway's
   non-cache streaming branch did not tee responses into the audit record.
   Agent skipped SSE responses entirely (`SSE excluded` comment).

E37 collapses the threshold into one knob, moves agent spill writes off
Hub via a pre-signed URL flow, and closes the four-quadrant coverage matrix
(stream / non-stream × request / response) uniformly across all three
data-plane services.

## Glossary

- **Inline body** — body bytes stored as JSONB on `traffic_event_payload.inline_*_body`.
- **Spill body** — body bytes stored as a file in the configured `SpillStore`
  backend (localfs in dev, S3 in prod); the row keeps a `*_spill_ref` JSON
  pointer.
- **`MaxInlineBodyBytes`** — the admin-tunable threshold. Bodies at or below
  this size go inline; larger bodies spill to a file. Replaces the historical
  `MaxBodyBytes` (truncation cap) and `InlineThresholdBytes` (YAML cutoff).
- **Pre-signed upload URL** — a one-shot URL Hub mints for an Agent that
  needs to upload a spill body. The URL points at S3 directly in prod or at
  a Hub-hosted blob endpoint in dev.
- **`perObjectCap`** — the storage-policy hard ceiling per spill object
  (default 256 MiB). Stays in YAML; not admin-tunable.

## Personas

- **Admin / Compliance reviewer** — toggles `storeRequestBody` /
  `storeResponseBody` per host in Admin UI; sets `Inline body size limit`
  to control DB cost vs file-storage cost.
- **Operator (DevOps)** — provisions the spill backend (localfs root or S3
  bucket) per environment via YAML; never edits the inline threshold (that
  is admin's domain).
- **Data-plane service owner** — implements `EmitBody` calls and capture
  buffers in AI-Gateway / Compliance-Proxy / Agent; relies on the unified
  threshold semantic to keep three services in lockstep.
- **Investigator (incident response)** — opens a traffic event in the UI
  and expects to see request + response bodies regardless of inline /
  spill storage and regardless of streaming / non-streaming origin.

## Functional Requirements

### F1. Single inline-vs-spill threshold (must)

- F1.1 — `payloadcapture.Config` exposes a single byte cap field
  `MaxInlineBodyBytes`. The historical name `MaxBodyBytes` is removed
  (no alias).
- F1.2 — Wire JSON for `system_metadata['payload_capture.config']`,
  Hub Cat B aggregation, and shadow push uses the key
  `maxInlineBodyBytes`. The historical key `maxBodyBytes` is removed.
- F1.3 — `spillstore.EmitBody` consumes `MaxInlineBodyBytes` as its
  threshold parameter. Bodies whose captured size `<= MaxInlineBodyBytes`
  are emitted inline; bodies `>` it are written via `SpillStore.Put` and
  the audit row keeps a `SpillRef`.
- F1.4 — `FactoryConfig.InlineThresholdBytes` is removed from the YAML
  schema and from the Go struct. Existing dev YAML files have the field
  deleted in the same change.
- F1.5 — `MaxInlineBodyBytes <= 0` is coerced to `DefaultMaxInlineBodyBytes`
  (256 KiB) by `DecodeConfigJSON` and by the Hub Cat B loader. The
  legacy "0 = unlimited" semantic is removed; downstream call sites no
  longer carry their own fallback constants.

### F2. Producer-only spill write — Hub never `Put`s (must)

- F2.1 — `nexus-hub/internal/handler/agent_audit.go` removes the
  `SpillStore` field, the `SpillThreshold` field, and every call to
  `spillstore.EmitBody`. The handler accepts only spill *references*
  from the agent, never raw bytes destined for spill.
- F2.2 — Agent stops embedding raw spill bytes in the HTTP audit
  envelope. When `len(body) > MaxInlineBodyBytes`, the agent obtains a
  pre-signed URL from Hub, uploads the body, and ships the resulting
  `SpillRef` in the audit envelope. When `len(body) <= MaxInlineBodyBytes`
  the agent inlines bytes as before (base64 in JSON).
- F2.3 — Hub exposes two new internal endpoints:
  - `POST /api/internal/things/spill-uploads` mints a one-shot upload
    URL plus an opaque `key` and the chosen `backend`.
  - `PUT /api/internal/spill/blob/:token` is the dev-mode (`localfs`)
    upload sink that streams the body into the local backend at the
    Hub-chosen key. In prod (`s3`) this endpoint is not registered;
    the URL points at S3 directly.
- F2.4 — The mint endpoint signs an HMAC token with the request scope
  (event id, direction, expected size, expected SHA-256, expiry).
  The blob endpoint validates the token signature, expiry, one-shot
  consumption (Redis dedup set), and `Content-Length` against the
  expected size before any disk write.

### F3. Four-quadrant capture coverage (must)

- F3.1 — AI-Gateway captures request + response, stream + non-stream.
  Cache-on and cache-off streaming branches share a single tee wrapper.
- F3.2 — Compliance-Proxy captures request + response on every event
  it emits, using the same `EmitBody` + `MaxInlineBodyBytes` flow as
  AI-Gateway. The previous "no clip / no spill awareness" path is
  removed.
- F3.3 — Agent captures request + response, stream + non-stream. The
  prior `SSE excluded` exception is removed; SSE response bodies flow
  through the same capture buffer as non-stream responses, bounded by
  `spill.perObjectCap`.
- F3.4 — Streaming captures whose live byte total exceeds
  `spill.perObjectCap` mid-flight stop buffering and stamp
  `Truncated=true` on the audit row. Client bytes continue to flow on
  the wire unchanged.

### F4. Reader symmetry — UI shape independent of inline-vs-spill (must)

- F4.1 — Control Plane's `GET /api/admin/traffic/:id` resolves
  `*_spill_ref` via `SpillStore.Get` and returns the body in the same
  shape it would have had if it were inline of the same content type.
- F4.2 — When `SpillRef.ContentType` indicates JSON (`application/json`
  or `+json`) and the resolved bytes parse as JSON, the body is
  returned as raw JSON. Otherwise (SSE, multipart, binary) the body is
  wrapped as a JSON string. This mirrors `decodeBodyEnvelope`'s
  raw / base64 split.
- F4.3 — A spill-resolution failure (backend unreachable, ref not
  found) returns the request with the spill_ref intact and a
  diagnostic indicator on the body field; the UI renders "stored
  externally — fetch failed" rather than blanking the row.

### F5. Admin UI rename (must)

- F5.1 — The admin UI form field previously labelled
  "Audit truncation cap (bytes)" is renamed to **"Inline body size
  limit (bytes)"** with the description "Bodies up to this size are
  stored inline in the database. Larger bodies are stored as files in
  the configured spill backend." All three locales (en, zh, es)
  update synchronously; `public/locales/` is mirrored.
- F5.2 — The form field key on the admin POST/PUT body is renamed to
  `maxInlineBodyBytes` end-to-end (Control Plane handler accepts the
  new key only).
- F5.3 — Tooltips, validation messages, and any column header that
  surfaced the old name update consistently.

## Non-Functional Requirements

### NFR-Performance

- N1 — `EmitBody` decision (inline vs spill) must be O(1) on the
  size comparison. No size-conditional double-allocation; producers
  hand a single `[]byte` to `EmitBody` and trust it.
- N2 — Streaming capture must not increase per-stream allocation by
  more than 1× the previous behavior. The unified tee wrapper grows
  its buffer up to `perObjectCap` lazily; it does not pre-allocate.
- N3 — Hub spill-upload mint endpoint must answer in `<10 ms` p99
  under a 100-rps burst (HMAC + Redis SET only).

### NFR-Security

- N4 — Spill upload tokens are HMAC-SHA-256 signed with a server-side
  secret derived from `system_metadata['hub.spill_upload_secret']`.
  Secret rotation is supported; tokens reference the secret epoch so
  in-flight URLs survive rotation up to `2 × maxTTL`.
- N5 — Tokens are one-shot; the dev `PUT /spill/blob/:token` endpoint
  rejects replays via Redis SETNX with TTL `2 × maxTTL`.
- N6 — Body upload is rejected if `Content-Length` differs from the
  expected size or if the streamed body's running SHA-256 differs
  from the expected SHA-256 (computed in chunks, no full re-read).
- N7 — Spill upload tokens cannot escalate scope: the `key` is
  generated by Hub from `(eventId, direction, sha256)` and signed in,
  preventing the agent from overwriting a prior event's blob.

### NFR-Reliability

- N8 — Spill upload failures (network, backend) propagate to the
  audit pipeline as an inline-fallback path: when an agent's PUT
  fails, the agent falls back to inlining a *truncated* body
  (`Truncated=true`) so the audit row is still emitted. The same
  fallback applies to AI-Gateway and Compliance-Proxy if their
  direct `SpillStore.Put` errors.
- N9 — Streaming captures whose buffer exceeds `perObjectCap` mid
  flight set `Truncated=true` on the audit row. Client bytes continue
  to flow unchanged.

### NFR-Observability

- N10 — Add Prometheus counters
  `payload_capture.spill.attempts_total{result, direction, source}`
  (result ∈ inline/spill/fallback/error). Existing
  `compliance_payload_capture_failure_rate` aggregator updates to
  read this metric.
- N11 — Hub mint and blob endpoints emit structured logs at INFO
  with `eventId`, `direction`, `sizeBytes`, `result`. Token secrets
  must never be logged.

### NFR-Compatibility

- N12 — Pre-GA: per CLAUDE.md "no backward compatibility, no defer".
  Old YAML field `spill.inlineThresholdBytes` is deleted, not
  deprecated. Old wire field `maxBodyBytes` is removed, not aliased.
  `system_metadata['payload_capture.config']` rows are reseeded as
  part of the change; no migration code is shipped.

## Constraints & Assumptions

- **C1** — Pre-GA, no installed user base. Wire / API / config breaks
  are acceptable in a single change.
- **C2** — `localfs` and `s3` are the only spill backends in scope.
  Azure Blob / GCS may follow as separate epics.
- **C3** — Agents talk to Hub over mTLS; the spill-upload endpoint
  trusts the agent's mutual TLS identity to authorise URL minting.
- **C4** — Redis is available for token dedup. If Redis is unreachable
  the mint endpoint returns `503` so the agent falls back to inline-
  truncated capture rather than risking replay.
- **C5** — `perObjectCap` is a YAML knob, not an admin knob. Operators
  size it per environment to bound disk / S3 PUT cost; admins should
  not be able to bump it via UI.

## MoSCoW Priority

| Req | Priority | Notes |
|---|---|---|
| F1 — Single threshold | Must | Primary D2 fix; everything downstream depends on this. |
| F2 — Producer-only spill | Must | D1 fix; pre-signed URL flow + Hub `agent_audit` cleanup. |
| F3 — Four-quadrant coverage | Must | New scope from this round (stream + non-stream × req + resp). |
| F4 — Reader symmetry | Must | UI single-shape requirement. |
| F5 — UI rename | Must | i18n update synchronous with backend rename. |
| N1, N2, N3 | Must | Performance budgets. |
| N4–N7 | Must | Security posture for the new upload endpoint. |
| N8 — Inline-fallback on Put failure | Must | Audit must not silently drop. |
| N9 — Stream cap truncation flag | Must | Required to expose oversized streams in UI. |
| N10–N11 | Should | Observability; ship in same change to avoid blind operations. |
| N12 — Hard break, no migration code | Must | CLAUDE.md "no backward compatibility" rule. |
| Incremental streaming spill (C2 from review) | Won't (this round) | In-memory bounded by `perObjectCap` is sufficient for current SSE traffic; revisit if future telemetry shows truncations >X% of streams. |

## Out of scope

- Azure Blob / GCS spill backends.
- Admin UI showing spill metrics (file count, total bytes) — covered
  by the existing `Stats()` plumbing; UI surface is a separate epic.
- Multi-region / cross-cluster Hub deployments where the mint Hub and
  the audit Hub differ — single-Hub topology is assumed.
