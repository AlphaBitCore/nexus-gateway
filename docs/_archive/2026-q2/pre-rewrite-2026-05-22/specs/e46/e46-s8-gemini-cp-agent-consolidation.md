# E46 S8 — Gemini normalizer + cp/agent fully on shared/normalize

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** (no new endpoints; consolidation only)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 7 adds Gemini coverage and removes any remaining in-package extractors in compliance-proxy and agent. All three data-plane services consume `packages/shared/transport/normalize/` exclusively for normalized payloads — there is no service-private duplicate.

Cross-service consistency is asserted by integration tests: an identical Anthropic Messages call captured by ai-gateway, compliance-proxy, and agent must produce byte-identical `NormalizedPayload` (modulo per-source metadata that is intentionally different, like the originating Thing ID).

---

## Story

### S8 — Gemini + consolidation

**User story:** As a Nexus platform engineer, the same wire bytes captured anywhere in the system produce the same normalized payload, with full coverage for Gemini in addition to OpenAI and Anthropic.

**Tasks:**

- **T8.1** — Implement `packages/shared/transport/normalize/gemini_generate.go`:
  - Request: `contents[].parts[]` → `messages[].content[]` (text / inline_data / file_data); `systemInstruction` → synthetic `system` message; `tools[]` mapping; sampling params.
  - Response (non-stream): `candidates[0].content.parts` → `output_messages[0].content[]`; `finishReason` → `finish_reason`; `usageMetadata` → `usage`.
  - Response (stream): assemble the chunked JSON-newline-delimited stream, concatenate parts by content type, capture last finishReason. Preserve `thoughts` parts as `reasoning` blocks.
- **T8.2** — Identify and delete any remaining in-package extractors:
  - `packages/compliance-proxy/internal/extract/` (if present) — superseded.
  - `packages/agent/core/extract/` (if present) — superseded.
  - Any references in their handlers re-route to `shared/normalize`.
- **T8.3** — Cross-service consistency test (`tests/integration/normalize_consistency_test.go`):
  - Send the same crafted Anthropic Messages request through ai-gateway, through compliance-proxy, and have an agent in `intercept-and-forward` mode see it. Compare `traffic_event_normalized.request_normalized` across all three rows; assert byte-equality on the JSON-serialized form (after sorting object keys and excluding per-source metadata fields).
  - Repeat for OpenAI Chat (S2) and Gemini Generate (this story).
- **T8.4** — Update `tests/run-all.sh` to include the new consistency tests.
- **T8.5** — Provider list in the catalog (`packages/shared/security/iam/catalog_data.go` only if a new resource is needed — likely not; normalize is internal).

**Acceptance:**

- **AC-S8.1** — A Gemini chat call (non-stream and stream) produces a `traffic_event_normalized` row with the expected `messages[]` and `usage`.
- **AC-S8.2** — Cross-service consistency test passes: same wire bytes → byte-identical NormalizedPayload across ai-gateway / compliance-proxy / agent.
- **AC-S8.3** — `grep -rln "extract\|Extractor" packages/compliance-proxy packages/agent` returns no hits referencing payload-content extraction (the only matches should be unrelated extractor patterns, e.g. credential extractors, which are documented as intentional).
- **AC-S8.4** — `tests/run-all.sh` passes locally with normalize consistency tests enabled.
