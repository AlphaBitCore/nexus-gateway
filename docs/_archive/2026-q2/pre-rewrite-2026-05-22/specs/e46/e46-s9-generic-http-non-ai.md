# E46 S9 — GenericHTTPNormalizer + non-AI traffic

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** (no new endpoints)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 8 closes the loop for compliance-proxy and agent: non-AI traffic (GitHub browsing, Slack, REST APIs, file downloads, binary uploads) is normalized via `GenericHTTPNormalizer` dispatching by `Content-Type`. Hooks gain `applicableTrafficKinds` filtering at the pipeline level so they only run against kinds they declare relevance for.

GenericHTTPNormalizer dispatches:

| Content-Type | kind | body_view |
| --- | --- | --- |
| `application/json`, `application/*+json` | `http-json` | Decoded JSON tree, plus a `text` projection (pretty-printed JSON) |
| `text/*` | `http-text` | Decoded text (assuming UTF-8 or charset hint), full content up to capture cap |
| `application/x-www-form-urlencoded` | `http-form` | Field/value map |
| `multipart/form-data` | `http-multipart` | Parts as a list; each part is recursively normalized (text part → inline; binary part → binary_ref) |
| Anything else | `http-binary` | `binary_ref { size, content_type, sha256 }` only — no inline content |

Binary kind has no text projection, so content-regex hooks naturally ABSTAIN on it.

---

## Story

### S9 — GenericHTTPNormalizer + applicableTrafficKinds

**User story:** As a security admin, my non-AI compliance-proxy / agent traffic is captured in a readable format, and my AI-specific hooks do not get triggered on it unless I opt them in.

**Tasks:**

- **T9.1** — Implement `packages/shared/transport/normalize/generic_http.go`:
  - `GenericHTTPNormalizer` implements `Normalizer`; checks `Content-Type` and dispatches.
  - JSON path: `json.Decode` to `any`; if encode error, return `(zero, ErrUnsupported)`. Body projection is the pretty-printed JSON; the structured tree is stored under `body_view.json` for the UI.
  - Text path: decode per charset hint (default UTF-8); if non-UTF-8 fails, return `ErrUnsupported`.
  - Form path: `url.ParseQuery` → field map.
  - Multipart path: `mime/multipart.Reader` walk parts; each text part inline up to per-part cap; binary parts captured by reference.
  - Binary path: only `size`, `content_type`, `sha256` (computed during capture and persisted on `traffic_event_payload`).
  - Headers: strip `Authorization`, `Cookie`, `x-api-key`, `proxy-authorization`. Whitelist the rest into `headers_filtered`.
- **T9.2** — Register `GenericHTTPNormalizer` as the fallback in the registry chain: provider-specific normalizer → GenericHTTPNormalizer → `unsupported`.
- **T9.3** — `packages/shared/compliance/pipeline.go`:
  - Filter hooks by `cfg.ApplicableTrafficKinds` against `NormalizedPayload.Kind` before execution. Skipped hooks do not appear in `traffic_event.request_hooks_pipeline` for that event.
- **T9.4** — Update UI: ensure the renderers from S7 handle all `http-*` kinds. Verify binary placeholder card is rendered correctly.
- **T9.5** — Storage retention decision: by default, non-AI traffic with `kind = http-binary` stores `request_normalized = {"kind":"http-binary","binary_ref":{…}}` (small payload, no inline). `http-json` / `http-text` stores the full normalized payload up to the same cap as `traffic_event_payload`. If an operator wants to drop content entirely for non-AI traffic, they set per-hook `storageAction = drop-content`; there is no separate global toggle.
- **T9.6** — Documentation: a short ops note in `docs/operators/ops/` describing non-AI traffic storage growth expectations and how to scope hooks to AI-only.
- **T9.7** — Integration tests: a compliance-proxy capture of (a) a GitHub HTML page browse, (b) a JSON REST API call, (c) a binary file download — verify each produces the expected kind and body_view, and AI-only hooks do not run against them.

**Acceptance:**

- **AC-S9.1** — A compliance-proxy-captured `application/json` REST call produces `kind = "http-json"` with the JSON tree visible in the UI.
- **AC-S9.2** — A binary file download produces `kind = "http-binary"` with only metadata in `request_normalized` / `response_normalized`.
- **AC-S9.3** — A PII-detector hook with `applicableTrafficKinds = ["ai"]` (default) does *not* run against any `http-*` kind — `traffic_event.request_hooks_pipeline` shows no entry for that hook on those events.
- **AC-S9.4** — A PII-detector hook with `applicableTrafficKinds = ["ai", "http-json", "http-text"]` runs against those kinds and reports matches.
- **AC-S9.5** — Sensitive headers (`Authorization`, `Cookie`, etc.) never appear in `traffic_event_normalized` rows even on `http-*` kinds.
