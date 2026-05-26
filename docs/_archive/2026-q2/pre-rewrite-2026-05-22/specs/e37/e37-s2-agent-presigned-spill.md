# E37-S2 — Agent Pre-Signed Spill Upload

Status: draft
Epic: E37 — Payload Capture Unification
Story: S2 — Agent uploads spill bodies via Hub-issued URLs; Hub never `Put`s

## 1. Problem

`packages/nexus-hub/internal/handler/agent_audit.go:181-190` calls
`spillstore.EmitBody(ctx, h.SpillStore, threshold, evt.PayloadRequest, …)`
on the agent HTTP audit-upload path. The agent ships raw base64 bytes in
the audit envelope; Hub then chooses inline-vs-spill and runs `Put`. This
violates the project rule that data-plane services own the spill decision
and the upload, with Hub acting only as a passive metadata sink.

## 2. Goal

After this story lands:

1. Agents stop embedding spill-sized bytes in the audit envelope. When
   `len(body) > MaxInlineBodyBytes`, the agent obtains a one-shot upload
   URL from Hub, PUTs the body to the URL target, then ships only the
   resulting `SpillRef` in the audit envelope.
2. Hub's `agent_audit` handler accepts spill *references* exclusively;
   `SpillStore` and `SpillThreshold` fields and the `EmitBody` call are
   removed. Hub never writes a spill file as part of audit ingestion.
3. Two new internal endpoints exist on Hub:
   - `POST /api/internal/things/spill-uploads` — mint URL + key.
   - `PUT /api/internal/spill/blob/:token` — dev-only localfs sink.
4. The reader symmetry from F4 ships in this story (the spill resolver
   sits next to the consumer plumbing, so it is convenient to land
   together).

## 3. Non-goals

- Threshold rename — covered in S1.
- Streaming-response coverage — covered in S3.
- Multi-region Hub topology — single-Hub assumption (C5 in
  Requirements).

## 4. Design

### 4.1 Flow

```
agent (data plane)                hub                          backend
------------------                ---                          -------
size_bytes = len(body)
if size_bytes <= MaxInlineBodyBytes:
    audit { requestBody: inline … }
    POST /agent-audit  ────────►
else:
    sha256 = SHA256Hex(body)
    POST /spill-uploads
       { eventId, direction,
         sizeBytes, contentType,
         sha256 }            ────►
                                 mint:
                                  - key = "agent/<yyyy-mm-dd>/<eventId>-<dir>"
                                  - token = HMAC( eventId | direction | key
                                                | sizeBytes | sha256
                                                | exp | epoch )
                                  - if backend == s3:
                                       presign PUT (TTL ≤5m,
                                       ContentLength fixed)
                                    elif backend == localfs:
                                       url = "https://hub/api/internal/spill/blob/<token>"
       { uploadUrl, key,
         backend, expiresAt } ◄──
    PUT uploadUrl  ────────────────────────────────────►  s3 OR hub→localfs
    audit {
      requestSpillRef: {
        backend, key, size,
        sha256, contentType }
    }
    POST /agent-audit  ────────►
                                 (handler ignores spill bytes;
                                  forwards SpillRef into MQ envelope)
```

### 4.2 Hub mint endpoint

Path: `POST /api/internal/things/spill-uploads`
Auth: mTLS thing identity (existing thing middleware)
Request:

```json
{
  "eventId": "uuid",
  "direction": "request" | "response",
  "sizeBytes": 12345678,
  "contentType": "application/json",
  "sha256": "lowercase hex"
}
```

Validation:
- `sizeBytes > 0` and `sizeBytes <= spill.perObjectCap` (else 413).
- `direction ∈ {request, response}` (else 400).
- `eventId` parses as UUID (else 400).
- `sha256` is 64 lowercase hex chars (else 400).

Response:

```json
{
  "uploadUrl": "https://...",
  "key": "agent/2026-05-06/<eventId>-request",
  "backend": "s3" | "localfs",
  "expiresAt": "2026-05-06T12:34:56Z"
}
```

The Hub-chosen `key` is signed into the token; the agent cannot
substitute another key.

### 4.3 Token shape

JWT-like compact: `<base64url(payload)>.<base64url(hmac)>`.

Payload fields:

```json
{
  "kid": "epoch-2",
  "eid": "<eventId>",
  "dir": "request",
  "key": "agent/2026-05-06/<eventId>-request",
  "sz": 12345678,
  "h": "<sha256>",
  "exp": 1746535696
}
```

HMAC over `payload` using the secret from
`system_metadata['hub.spill_upload_secret'].secrets[<kid>]`. Multiple
secrets coexist for rotation; mint always picks the newest. Verify
accepts any non-expired secret in the map.

`exp` clamped to `now + maxTTL` where `maxTTL = 5m`.

### 4.4 Hub localfs blob endpoint (dev only)

Path: `PUT /api/internal/spill/blob/:token`
Auth: token-only (no mTLS — the agent uploading has already been
  authenticated to mint the token)
Registered only when `cfg.Spill.Backend == "localfs"`.

Steps:
1. Decode token; verify HMAC against any non-expired secret in the
   map; fail with 401 on invalid or expired.
2. SETNX `spill_token_used:<kid>:<eid>:<dir>` in Redis with TTL
   `2 × maxTTL`. Existing key → 409 (token already consumed).
3. Reject if `Content-Length != payload.sz` (411 / 413 as
   appropriate).
4. Stream body into the `localfs` backend writer at the signed
   `key`. Compute running SHA-256; abort + delete partial on
   mismatch with `payload.h` (400).
5. Stream completion → 204.

The chunked SHA-256 verification ensures Hub never writes a payload
whose hash doesn't match what the agent registered, so a malicious
agent cannot trick Hub into hosting arbitrary blobs at chosen keys.

### 4.5 Hub `agent_audit` cleanup

Delete from `AgentAuditAPI`:

- The `SpillStore` field.
- The `SpillThreshold` field.
- The two `spillstore.EmitBody` calls (lines 186, 189).
- The `spillfactory` import.

Add to `AgentAuditEvent`:

- `RequestSpillRef *audit.SpillRef \`json:"requestSpillRef,omitempty"\``
- `ResponseSpillRef *audit.SpillRef \`json:"responseSpillRef,omitempty"\``

Forward into the MQ envelope as:

- if `evt.PayloadRequest != nil`:
  `envelope["requestBody"] = audit.NewInlineBody(evt.PayloadRequest, …)`
- elif `evt.RequestSpillRef != nil`:
  `envelope["requestBody"] = audit.NewSpillBody(evt.RequestSpillRef, …)`
- else: omit (`audit.EmptyBody()`)

### 4.6 Agent `spilluploader` package

`packages/agent/core/observability/spilluploader/uploader.go`:

```go
type Uploader interface {
    // Upload writes body bytes via a Hub-issued one-shot URL and
    // returns the resulting SpillRef. Returns ErrFallbackInline when
    // the upload could not be completed and the caller should fall
    // back to inlining a truncated body.
    Upload(ctx context.Context, eventID, direction, contentType string,
           body []byte) (audit.SpillRef, error)
}
```

Implementation:
1. Compute `sha256` of `body`.
2. POST to Hub mint endpoint with `{eventId, direction, sizeBytes, contentType, sha256}`.
3. PUT body to `uploadUrl` with `Content-Length: sizeBytes` and
   `Content-Type: contentType`. Use the agent's existing HTTP client
   so existing retry / timeout / cert-pinning policies apply.
4. On 2xx: return `SpillRef{Backend, Key, Size, SHA256, ContentType}`.
5. On any error after `maxRetries`: return `ErrFallbackInline`.

The agent's `intercept/payload_capture.go` becomes:

```go
func CaptureRequestBody(ctx context.Context, store *payloadcapture.Store,
                        uploader spilluploader.Uploader,
                        eventID, contentType string, body []byte,
                        ) (inline []byte, ref *audit.SpillRef) {
    if store == nil || len(body) == 0 { return nil, nil }
    cfg := store.Get()
    if !cfg.StoreRequestBody { return nil, nil }
    if int64(len(body)) <= cfg.MaxInlineBodyBytes {
        return body, nil
    }
    spilled, err := uploader.Upload(ctx, eventID, "request", contentType, body)
    if err != nil {
        // Fallback: inline a truncated copy so the audit row still emits.
        return body[:cfg.MaxInlineBodyBytes], nil
    }
    return nil, &spilled
}
```

Symmetrical `CaptureResponseBody`.

`TruncateBody` is removed; truncation only happens on the fallback
path now, controlled by the same `MaxInlineBodyBytes` value.

### 4.7 Reader symmetry — `resolveSpillBody`

`control-plane/internal/handler/admin_traffic.go::resolveSpillBody`
becomes ContentType-aware:

```go
func (h *AdminHandler) resolveSpillBody(ctx context.Context,
    refJSON []byte) (json.RawMessage, error) {
    var ref sharedaudit.SpillRef
    if err := json.Unmarshal(refJSON, &ref); err != nil { … }
    rc, err := h.SpillStore.Get(ctx, ref)
    if err != nil { … }
    defer rc.Close()
    body, err := io.ReadAll(rc)
    if err != nil { … }
    if isJSONContentType(ref.ContentType) && json.Valid(body) {
        return json.RawMessage(body), nil
    }
    out, err := json.Marshal(string(body))
    if err != nil { … }
    return out, nil
}

func isJSONContentType(ct string) bool {
    base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
    return base == "application/json" ||
           strings.HasSuffix(base, "+json")
}
```

This keeps the UI shape identical regardless of inline/spill storage:
JSON bodies render as objects, SSE / binary bodies render as strings.

### 4.8 `system_metadata['hub.spill_upload_secret']`

Schema:

```json
{
  "active": "epoch-2",
  "secrets": {
    "epoch-1": "<base64-secret>",
    "epoch-2": "<base64-secret>"
  }
}
```

Hub Mint always signs with `secrets[active]`. Hub Verify accepts any
secret whose key matches `payload.kid` and whose token is not
expired. A rotation procedure adds `epoch-3`, flips `active`, and
removes `epoch-1` after `2 × maxTTL`.

A bootstrap helper seeds a single random `epoch-1` secret on first
boot; production rotation is an admin runbook (out of scope).

## 5. Tasks

- T1 — `shared/audit.SpillRef` already carries `ContentType`; add a
  helper `IsJSONLike()` returning true when `ContentType` is JSON.
- T2 — Hub mint endpoint: handler, validation, HMAC sign,
  Redis-free path (dedup happens at consume time, not mint).
- T3 — Hub blob endpoint: handler, HMAC verify, Redis dedup,
  streamed SHA-256, write-through to `SpillStore.Put`.
- T4 — Hub `agent_audit` cleanup: delete `SpillStore` field and
  `EmitBody` calls; add `RequestSpillRef` / `ResponseSpillRef`
  fields; emit `audit.NewInlineBody` or `audit.NewSpillBody` into
  the MQ envelope.
- T5 — Agent `spilluploader` package + wiring in
  `intercept/payload_capture.go` (request and response).
- T6 — Agent audit envelope (`internal/audit/event.go`) gains the
  two SpillRef fields; the queue ↔ HTTP serializer round-trips them.
- T7 — Hub `cmd/nexus-hub/main.go` registers the two routes,
  injects `SpillStore`, `Redis`, `SecretLoader`.
- T8 — Control Plane `resolveSpillBody` ContentType-aware fork.
- T9 — `system_metadata` seed for the upload secret. `tools/db-migrate/seed/`.
- T10 — End-to-end test: agent uploads a 1 MiB request body,
  audit row carries spill_ref, Control Plane GET resolves and
  returns the body.

## 6. Acceptance criteria

- AC1 — `git grep "spillstore.EmitBody" packages/nexus-hub` returns
  zero hits.
- AC2 — `git grep "SpillStore" packages/nexus-hub/internal/handler/agent_audit.go`
  returns zero hits.
- AC3 — Posting `agent-audit` with `payloadRequest` of 100 KiB
  succeeds; the resulting traffic_event_payload row has
  `inline_request_body` set and `request_spill_ref IS NULL`.
- AC4 — Posting `agent-audit` with `requestSpillRef = {…, key:
  "agent/.../spilled.bin", …}` succeeds; the row has
  `request_spill_ref` set and `inline_request_body IS NULL`.
- AC5 — Mint endpoint rejects `sizeBytes > spill.perObjectCap` with
  413.
- AC6 — Blob endpoint rejects a replay (same token used twice) with
  409 on the second PUT.
- AC7 — Blob endpoint rejects a body whose streamed SHA-256 differs
  from the token's signed `h` with 400 and deletes the partial file.
- AC8 — Control Plane GET on a spill-stored event returns
  `responseBody` as raw JSON when ContentType is `application/json`
  and as a JSON string when ContentType is `text/event-stream`.

## 6.5 Implementation phasing (work order, not compat layer)

This story carries two implementation stages so the "Hub never Puts"
invariant lands in one shot but the production-only pre-signed URL
infrastructure (HMAC token, Redis dedup, mint+blob endpoints) does
not block the rename + four-quadrant coverage from S1+S3:

- **Stage 1 (this round)**: agents get their own `spillstore.Config`
  YAML block plus a `payloadcapture/intercept` integration that calls
  `spillstore.EmitBody` locally. Producer-side spill writes happen
  on the agent host directly. Hub's `agent_audit.go` removes the
  `SpillStore` field and the `EmitBody` calls (T4); the audit
  envelope newly accepts `requestSpillRef` / `responseSpillRef`. The
  audit MQ pipeline ingests refs end-to-end. **Dev (localfs)** works
  today; **prod (S3)** is supported with the agent embedding S3
  credentials.
- **Stage 2 (follow-up)**: the mint endpoint + dev localfs blob
  endpoint + HMAC token + Redis dedup ship for prod environments
  where operators do not want every agent to carry S3 IAM
  credentials. Until Stage 2 lands, prod deployments either use the
  Stage-1 agent-credentials path or stick with localfs spill.

The wire shape is identical between Stage 1 and Stage 2 — the agent
posts the same `{requestSpillRef, responseSpillRef}` envelope; only
the upload mechanism differs. So Stage 2 is a drop-in replacement
for the agent's `spilluploader` (Hub-mediated URL fetch instead of
direct backend Put), not a Hub or Control-Plane change.

## 7. Risks

- **R1 — HMAC secret leak**: a leaked secret lets a third party mint
  upload URLs. Mitigated by short token TTL (5m), one-shot
  consumption, and rotation runbook.
- **R2 — Local backend HTTP DoS**: agents could spam
  `/spill/blob/:token` with bogus tokens. Mitigated by validating
  HMAC before any disk I/O, by rate-limiting at the gateway layer,
  and by 5m TTL.
- **R3 — Redis unavailable**: dedup SETNX cannot run, so the
  endpoint must reject with 503 to avoid replay risk. Agent's
  `spilluploader` falls back to inline-truncated capture (per
  NFR-Reliability N8).
- **R4 — Pre-signed S3 URL with mismatched Content-Length**: AWS
  rejects, the agent retries; if persistent, the agent falls back
  to inline-truncated.
