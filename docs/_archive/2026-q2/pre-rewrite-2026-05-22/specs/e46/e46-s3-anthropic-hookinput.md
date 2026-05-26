# E46 S3 — Anthropic normalizer + atomic HookInput swap

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s3-anthropic-hookinput.yaml](../openapi/e46-s3-anthropic-hookinput.yaml)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 2 adds the AnthropicMessagesNormalizer and performs the breaking swap of `HookInput.Content []ContentBlock` → `HookInput.Normalized *NormalizedPayload`. All eleven shared hook implementations migrate to the new shape in a single atomic change. Per project policy, there is no parallel-run period.

The hook framework gains a helper `NormalizedTextProjection(*NormalizedPayload) []string` that returns the flat text fragments hooks operate on — one entry per content block of type `text`, `tool_result`, or (kind-`http-*`) the decoded `body_view.text`. Hooks that need finer-grained inspection (e.g. system vs user role separation) read `NormalizedPayload.messages[]` directly.

---

## Story

### S3 — Anthropic normalizer + HookInput swap

**User story:** As a Nexus platform engineer, I want every shared hook to consume the same `NormalizedPayload` regardless of provider so I can build new hooks without learning each provider's wire format.

**Tasks:**

- **T3.1** — Implement `packages/shared/transport/normalize/anthropic_messages.go`:
  - Map `/v1/messages` request body → `NormalizedPayload` (kind=`ai-chat`).
  - `system` field → a synthetic `messages[0]` with role=`system`.
  - `messages[]` content: text blocks → `ContentBlock{type:text}`; `image` blocks → `image_ref`; `tool_use` → `tool_use`; `tool_result` → `tool_result`.
  - Response normalize (non-stream): `content[]` → `output_messages[0].content[]`; `stop_reason` → `finish_reason`; `usage` → `usage{prompt_tokens, completion_tokens, cache_creation, cache_read}`.
  - Response normalize (stream): assemble Anthropic event stream (`message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`). **Preserve `thinking` content blocks** as `ContentBlock{type:"reasoning"}` rather than dropping.
- **T3.2** — Swap `HookInput.Content []ContentBlock` → `HookInput.Normalized *NormalizedPayload` in `packages/shared/policy/hooks/types.go`. Delete the old `Content` field and the `NormalizedToContentBlocks` helper.
- **T3.3** — Add `packages/shared/policy/hooks/projection.go` with `NormalizedTextProjection(*NormalizedPayload) []string`. Hooks that scan text use this helper.
- **T3.4** — Migrate every hook implementation in `packages/shared/policy/hooks/`:
  - `pii_detector.go`, `keyword_filter.go`, `content_safety.go`, `rulepack_engine.go` — switch from `input.Content` iteration to `NormalizedTextProjection(input.Normalized)` iteration.
  - `rate_limiter.go`, `request_size.go`, `ip_access.go`, `data_residency.go` — no content reads; minor type-only update to compile under the new `HookInput`.
  - `quality_checker.go` — reads response `output_messages[].finish_reason` directly from `input.Normalized` rather than today's heuristics on `Content`.
  - `webhook_forward.go` — sends `NormalizedPayload` (or a projection per `payloadMode`; the field itself ships in S4) in its HTTP body.
  - `noop.go` — type-only update.
- **T3.5** — Update `packages/shared/compliance/pipeline.go`:
  - `executeSequential` — when a hook returns `Modify` with non-empty `ModifiedContent`, today it patches `input.Content`. The new code patches `input.Normalized.messages[].content[]` per the returned `RedactionSpan[]` (the RedactionSpan plumbing itself lands in S5; in S3 the hook can still emit `Modify` with full content replacement as a transitional shape — but S5 deletes that path). For S3, keep the spans field stubbed in the type and have the pipeline accept either a `ModifiedContent` (transitional) or a `RedactionSpans` (final). Mark this clearly in the code so S5 can collapse it.
  - `UpstreamTags` propagation untouched.
- **T3.6** — Update every hook unit test to construct `HookInput` with `Normalized: &NormalizedPayload{…}` instead of `Content: []ContentBlock{…}`. Add fixtures for Anthropic-shaped inputs in addition to OpenAI-shaped (so the same hook is exercised against both shapes via projection).
- **T3.7** — Wire ai-gateway, compliance-proxy, and agent adapters to populate `HookInput.Normalized`. ai-gateway uses the result it already computed in S2. cp/agent reuse the same shared registry; for non-AI traffic the `GenericHTTPNormalizer` returns ErrUnsupported until S9 and the pipeline records the failure but lets the hook see an empty projection (effectively ABSTAIN).
- **T3.8** — Anthropic normalizer unit tests: representative fixtures (non-stream, stream, with tools, with `thinking` blocks, with cache_control markers, with multimodal image_ref), pathological cases.

**Acceptance:**

- **AC-S3.1** — `HookInput.Content` does not exist after this story; `grep -rn "HookInput.*Content\b\|input\.Content\b" packages/` returns zero hits outside historical comments.
- **AC-S3.2** — All hook unit tests pass under `go test -race -count=1`.
- **AC-S3.3** — A streaming Anthropic response with `thinking` blocks results in `output_messages[0].content` containing at least one `{type:"reasoning"}` block. The text projection does *not* include the reasoning text (by default — `applicableTrafficKinds` and content-type filters refine this in later phases).
- **AC-S3.4** — A PII-detector test that previously hit on a regex spanning two `Content` items now hits on the same regex applied to the projected text — i.e. PII fragmented across SSE chunks is now caught.
- **AC-S3.5** — `traffic_event_normalized` rows produced by all three services (ai-gateway, compliance-proxy, agent) for an identical Anthropic request body are byte-identical (modulo timestamps).
