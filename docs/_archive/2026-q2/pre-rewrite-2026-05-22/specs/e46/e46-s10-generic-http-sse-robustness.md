# E46 S10 — GenericHTTPNormalizer SSE/NDJSON robustness + audit-side response Content-Type stamping

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** (no new endpoints)
**Status:** Approved
**Date:** 2026-05-15

---

## Problem

Captured traffic from consumer surfaces (chatgpt.com `/backend-api/f/conversation`, claude.ai web turns, cursor unary calls, gemini-web) lands in the audit pipeline with `adapter_type` values that are *not* registered against any AI normalizer (`chatgpt-web`, `claude-web`, …). It also lands with no `path` match in the path-only fallback table. The lookup falls through to `*:*:*` `GenericHTTPNormalizer`.

For SSE responses (every ChatGPT chat turn), one of two failure modes triggers:

1. The audit envelope stamped `Content-Type=application/json` (because the producer either copied the request CT to the response body container or hardcoded empty → registry meta carries a default). `GenericHTTPNormalizer.Normalize` matches `application/json`, calls `normalizeJSON`, and `json.Unmarshal` fails on the SSE first byte `e` (from `event: delta_encoding…`). The error branch keeps `Kind=http-json` and writes `BodyView.Text=raw` (no JSON). UI's `http-json` render reads `BodyView.JSON` only → renders `(empty)` + a `partial` banner showing the cryptic decode error.
2. Even when CT is absent, the existing UTF-8-text fallback produces a usable text dump, but `http-json` UI rendering keeps drawing empty whenever a stored row carries the legacy partial kind.

A second structural issue: compliance-proxy's `emitter.go` hardcodes `""` for both request and response Content-Type when building the spillstore body container (lines 228–229). Even after this normalizer fix, the audit envelope still loses CT, which makes downstream debugging harder and prevents any future per-content-type analytics. Agent and ai-gateway audit paths already pass response CT correctly.

## Goals (in scope)

- Make `GenericHTTPNormalizer` recognise SSE and NDJSON bodies by **byte sniffing**, regardless of `meta.ContentType`, so even mis-stamped envelopes produce a readable text projection.
- Make the JSON-decode error branch produce a coherent `Kind=http-text` payload (rather than `Kind=http-json` with only `BodyView.Text`).
- Stamp the actual upstream response Content-Type onto the audit envelope from compliance-proxy so future captures carry truthful CT.
- Small UI defence: when `http-json` rows have only `BodyView.Text`, fall back to rendering text instead of `(empty)`. Covers historical broken rows that pre-date this fix.

## Non-goals (explicitly out)

- AI-protocol-aware extraction (assistant text from `event: delta` payloads, Usage / token counts) for `chatgpt-web`, `claude-web`, `cursor`, `gemini-web`. Consumer surfaces have no stable wire schema; per [[feedback_compliance_proxy_text_first]] text-only extraction is the agreed goal at this stage. Token / model / usage info may stay empty for these adapters.
- New `KindHTTPEventStream`. SSE projects to `KindHTTPText` (verbatim dump) for now; UI's text renderer already handles it.
- Backfill of historical `traffic_event_normalized` rows. The byte-sniff fix makes any future re-normalize succeed; an admin-triggered backfill is a follow-up if operators want existing rows refreshed.
- Audit-side response-header propagation outside compliance-proxy. Agent already plumbs `PayloadResponseContentType` correctly; ai-gateway already passes `ResponseContentType`. Only compliance-proxy needs the fix.

## Story

### S10 — Generic-HTTP SSE/NDJSON byte-sniff + audit CT stamping

**User story:** As a security admin reviewing a captured ChatGPT chat turn (or any consumer-LLM web SSE), I see the readable conversation text in the Traffic Detail's Normalized panel instead of an empty body + cryptic parse error.

**Tasks:**

- **T10.1** — `packages/shared/transport/normalize/generic_http.go`:
  - Add `looksLikeSSE(raw []byte) bool` — first non-whitespace bytes start with `event:` or `data:` (covers ChatGPT, OpenAI, Anthropic, generic SSE).
  - Add `looksLikeNDJSON(raw []byte) bool` — at least two non-empty lines, each independently `json.Unmarshal`-able into `any`, no leading `[` (which would be a real JSON array).
  - Modify `Normalize`: run sniffers *before* the content-type switch. SSE → `normalizeSSE`; NDJSON → `normalizeNDJSON`.
  - Add `normalizeSSE(raw)` — emit `Kind=KindHTTPText` + `BodyView.Text=string(raw)` (verbatim). No error.
  - Add `normalizeNDJSON(raw)` — decode each non-empty line, emit `Kind=KindHTTPJSON` + `BodyView.JSON=[]any{…}`. If any line fails decode, fall through to `normalizeText`.
  - Modify `normalizeJSON` error branch: when `json.Unmarshal` fails, try (a) SSE sniff, (b) NDJSON sniff, (c) UTF-8 text sniff, (d) binary ref. Set `Kind` accordingly. Stop returning a non-nil error for shapes we successfully routed to text — those are *not* "partial", they are "JSON content-type was a lie, the body is plain text".

- **T10.2** — `packages/compliance-proxy/internal/compliance/emitter.go`:
  - Add `ResponseContentType string` to `AuditInfo` struct.
  - In `buildEvent`, pass `getContentType(info.Headers)` (request side) and `info.ResponseContentType` (response side) to `spillstore.EmitBody` instead of empty strings.
  - Update SSE + non-SSE call sites in `proxy/sse.go` + `proxy/forward_handler.go` to stamp `auditInfo.ResponseContentType = resp.Header.Get("Content-Type")` before calling `Emit` / `EmitDual`.

- **T10.3** — `packages/control-plane-ui/src/pages/traffic/NormalizedPayloadView.tsx`:
  - In the `http-json` render branch, when `bodyView.json == null` but `bodyView.text` non-empty → render the text fallback (same monospace `<pre>` used by `http-text`).

- **T10.4** — Tests in `packages/shared/transport/normalize/generic_http_test.go`:
  - Table-driven: ChatGPT SSE fixture (truncated from `baa07c15` real bytes), Anthropic-shaped SSE, NDJSON, real `application/json` body, plain text body, garbage binary, gemini-style ndjson array body.
  - Verify each: `Kind`, `BodyView.Text` / `BodyView.JSON` populated, no error returned (except for binary ref).

## Acceptance criteria

- **AC-S10.1** — A `meta.ContentType=application/json` envelope with SSE bytes (`event: delta\ndata: …`) returns `Kind=http-text`, `BodyView.Text=<full SSE>`, **no error**.
- **AC-S10.2** — A `meta.ContentType=text/plain` envelope with NDJSON body returns `Kind=http-json`, `BodyView.JSON` is a 2+ element array of decoded objects.
- **AC-S10.3** — A legitimate `application/json` body still routes to `Kind=http-json`, `BodyView.JSON` populated (no regression).
- **AC-S10.4** — After deploying compliance-proxy with T10.2, a fresh ChatGPT chat turn produces a `traffic_event_normalized` row where the audit pipeline stamped the actual upstream response CT (`text/event-stream`) into `spillstore.Body.ContentType`. Verified via DB inspection of the spillstore container metadata.
- **AC-S10.5** — UI shows the readable SSE dump for a re-normalized `baa07c15`-equivalent row instead of `(empty)`.
- **AC-S10.6** — UI's defence-in-depth render: legacy rows with `Kind=http-json` + `BodyView.text` only also render readable.

## Rollout

No phased rollout (per CLAUDE.md "no backward compatibility"): both backend + audit + UI changes ship together. Historical broken rows stay broken unless an operator triggers a backfill (out of scope). New traffic from the moment compliance-proxy restarts is correct end-to-end.
