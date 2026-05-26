# E46 S5 — Transformation tracking framework + cp/agent inflight redact

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s5-redaction.yaml](../openapi/e46-s5-redaction.yaml)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 4 promotes `TransformSpan` (formerly `RedactionSpan` — the alias is retained for narrow APIs) to the canonical mechanism for every byte-level modification Nexus made between the client and the upstream. The set of recorded modification sources expands beyond hooks:

- `hook` — content-touching hook redact (pii-detector / keyword-filter / content-safety / rulepack-engine).
- `aiguard` — LLM-as-judge classifier suggested a redact span.
- `cache-normaliser` — E38 prompt-cache normaliser stripped volatile bytes from the request body before sending upstream.
- `cache-control-inject` — E38 cache_control marker injection.
- `cache-key-strip` — Nexus L1 cache-key normalisation removed volatile bytes for the cache key computation only (upstream body unaffected; recorded for audit completeness).

This makes the audit log byte-level reproducible: given `request_normalized` (the **original client request**) plus `request_transform_spans`, an auditor can reconstruct the **upstream-bound body** exactly. Similarly for the response side: `response_normalized` (the **original upstream response**) plus `response_transform_spans` reconstructs the **client-bound body**.

The transitional `ModifiedContent` field on `HookResult` (kept in S3 for compile compatibility) is deleted. Pipeline post-processing applies spans to the stored normalized copy unconditionally, and to the upstream body when the hook's `onMatch.inflightAction = redact` and the protocol's `TrafficAdapter` supports `RewriteRequestBody`.

compliance-proxy and agent pipelines flip `allowModify = true` and route through the same `TrafficAdapter.RewriteRequestBody` interface used by ai-gateway. The legacy `MODIFY_DOWNGRADED_TO_REJECT` branch in `packages/shared/compliance/pipeline.go` is deleted (unreachable).

Protocols whose mid-stream rewrite is unsafe declare `ErrRewriteUnsupported` on their adapter; the pipeline catches this, downgrades to storage-only redact, and emits `ReasonCode = REDACT_INFLIGHT_UNSUPPORTED` via the existing `HookResult.Reason` / `ReasonCode` channel. No schema additions.

---

## Story

### S5 — RedactionSpan canonicalization + cp/agent enablement

**User story:** As a compliance officer, I see hooks redact content the same way on all three services: spans are visible in the audit log, upstream calls receive a rewritten body when the protocol supports it, and the UI tells me which protocol/scenario caused a downgrade.

**Tasks:**

- **T5.1** — Delete `HookResult.ModifiedContent` from `packages/shared/policy/hooks/types.go`. Replace with `TransformSpans []normalize.TransformSpan` (hook-emitted spans carry `Source = "hook"` and `SourceID = rule.ID`).
- **T5.2** — `packages/shared/compliance/pipeline.go`:
  - Delete the `MODIFY_DOWNGRADED_TO_REJECT` branch (lines ~269-281 in the pre-E46 file).
  - Replace the "patch `input.Content`" path with "patch `input.Normalized` by applying spans" — a small helper `normalize.ApplySpans(*NormalizedPayload, []RedactionSpan)` in shared/normalize.
  - After the pipeline completes, write the final `NormalizedPayload` (with spans applied per `storageAction`) into `traffic_event_normalized.{request_normalized,response_normalized}` and the raw `RedactionSpan[]` into the `redaction_spans` JSONB column.
  - On `inflightAction = redact`, call `trafficAdapter.RewriteRequestBody(ctx, originalBody, path, spans)`; on `ErrRewriteUnsupported`, log a warn, mark `HookResult.ReasonCode = "REDACT_INFLIGHT_UNSUPPORTED"`, leave the original body forwarded.
  - `SetAllowModify(true)` becomes the default in cp/agent pipeline constructors.
- **T5.3** — `packages/shared/traffic/` adapter interface gains explicit `ErrRewriteUnsupported` semantics. Each existing adapter (OpenAI, Anthropic) implements `RewriteRequestBody` for the request stage. Response-stage rewrite is supported only for non-stream responses; streaming responses always return `ErrRewriteUnsupported` (the chunks have already been forwarded by the time the post-stream hook fires). This is documented and reflected in the UI banner.
- **T5.4** — `packages/compliance-proxy/internal/proxy/forward_handler.go` and `packages/agent/...` enable `allowModify=true` on their pipeline constructors. Verify with integration tests that a redact hook actually rewrites the request body forwarded to the upstream.
- **T5.5** — `packages/shared/transport/normalize/apply_spans.go`:
  - `ApplySpans(p *NormalizedPayload, spans []RedactionSpan) *NormalizedPayload` returns a new payload with the byte ranges in the addressed content blocks replaced. Idempotent.
  - Spans are addressed via `(direction, message_index, content_index, start, end)` — the structure tolerates content reorganization (so long as the addressed block still exists).
- **T5.6** — Update existing AI-Guard response decoding (still ai-gateway-internal at this phase) to emit `TransformSpan[]` (with `Source = "aiguard"`) instead of the single-string `modified_content`. The judge prompt is updated to ask for structured span output.
- **T5.8** — Wire E38 cache normaliser to emit `TransformSpan[]` (`Source = "cache-normaliser"` for strips that affect upstream, `Source = "cache-key-strip"` for L0 strips that affect only the cache key). The existing `NormalizedStripCount` / `NormalizedStripBytes` counters remain for fast aggregate queries but the span set carries the byte-level detail compliance reviewers need.
- **T5.9** — Wire E38 cache_control injection to emit `TransformSpan[]` (`Source = "cache-control-inject"`, `Action = "inject"`). The existing `CacheMarkerInjected` counter remains.
- **T5.7** — `traffic_event.request_hook_reason_code` and `response_hook_reason_code` accept the new `ReasonCode` strings. No schema change (the column is `TEXT`).

**Acceptance:**

- **AC-S5.1** — A hook configured with `onMatch.inflightAction = "redact"` running against a non-streaming OpenAI chat request: the request forwarded to the upstream provider contains the redacted text; the `traffic_event_normalized.request_normalized` reflects the same redaction; `request_redaction_spans` has one entry per applied span.
- **AC-S5.2** — The same hook against a streaming chat response: `inflight` is *not* applied (stream already forwarded), `storage` IS applied, `response_hook_reason_code = "REDACT_INFLIGHT_UNSUPPORTED"`, `response_redaction_spans` has the entries.
- **AC-S5.3** — `storageAction = "drop-content"` results in `traffic_event_normalized.request_normalized = {"redacted":true,"kind":<kind>,"rule_ids":[…]}` and `request_hook_reason_code = "STORAGE_DROPPED_BY_POLICY"`.
- **AC-S5.4** — `grep -rn "MODIFY_DOWNGRADED_TO_REJECT\|ModifiedContent\b" packages/` returns zero hits after this story.
- **AC-S5.5** — compliance-proxy integration test: a Cursor → Anthropic-proxied call with a PII match yields a redacted body sent upstream (Anthropic receives `[REDACTED_EMAIL]` instead of the email) AND the audit row has the same redaction applied.
- **AC-S5.6** — agent integration test: same as AC-S5.5 but with the agent capture path.
- **AC-S5.7** — A protocol declared `ErrRewriteUnsupported` results in upstream receiving the original body, audit log showing the redacted version, and the UI Normalized tab displaying the banner "Redaction applied to audit log only — this protocol's mid-stream rewrite is unsafe."
