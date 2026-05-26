# E62-S1 — Hook Framework Endpoint + Modality Gating

> Story: e62-s1
> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Requirements: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md` §FR-1, §FR-9.5
> Architecture: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` §6 (hook framework endpoint+modality awareness), §8.7.2 (three-path applicability matrix); `docs/developers/architecture/services/ai-gateway/hook-architecture.md` (extended scope note at top); `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a Rule 6 (hooks parity)
> Memory: `project_e62_cross_adapter_embeddings` (to be created), cross-ref `feedback_compliance_proxy_text_first`
> Blocked by: none (foundational; can ship independently of S2-S5)
> Blocks: S6 (CP/Agent reuse the same framework — gating semantics must be agreed first)

---

## User Story

As a **gateway architect maintaining the shared hook framework**, I want `HookInput` to carry `EndpointType` + `Modality`, `Hook` to declare `SupportsEndpoint()` + `SupportsModality()`, and `Pipeline.BuildPipeline` to filter at build time — so that adding non-chat endpoints (embeddings in E62; image / audio / video in later epics) does not silently run content-scanning hooks on bodies that carry no text content, and so that audit rows correctly distinguish "no applicable hooks" from "hooks approved the request".

Co-personas:
- **AI Gateway operator** — embedding responses stop producing misleading `decision=APPROVE` rows when no Class-A hook had anything to evaluate.
- **Compliance Proxy operator** — embedding traffic transiting CP gets the same Class-A skipping behaviour for free (shared code path; FR-9.5).
- **Hook implementer** — every new hook explicitly declares which endpoints + modalities it applies to. No silent "supports everything by default".

---

## Tasks

### T1 — Extend `HookInput` and `Hook` interface

- T1.1 In `packages/shared/policy/hooks/core/types.go`, add three new fields to `HookInput`:
  - `EndpointType EndpointType`
  - `InputModality []Modality`
  - `OutputModality []Modality`
- T1.2 Define `EndpointType` enum: `chat`, `embeddings`, `image_generation`, `tts`, `stt`, `video_generation`, `batch`, `job`. Constants `EndpointTypeChat`, `EndpointTypeEmbeddings`, etc. Place in the same file as `HookInput` so consumers import one symbol path.
- T1.3 Define `Modality` enum: `text`, `image`, `audio`, `video`. Constants `ModalityText`, etc.
- T1.4 Extend the `Hook` interface with two methods:
  - `SupportsEndpoint(EndpointType) bool`
  - `SupportsModality(Modality) bool`
- T1.5 Provide a default helper `ChatOnly()` that wraps a hook and returns `SupportsEndpoint(t) → t == EndpointTypeChat`, `SupportsModality(m) → m == ModalityText`. Existing hooks that need to compile against the new interface without behaviour change embed `ChatOnly` and are explicitly audited in T2 to either keep `ChatOnly` (correct semantics) or declare wider applicability.

### T2 — Audit existing built-in hooks and declare applicability

For each built-in hook under `packages/shared/policy/hooks/<hook>/`, decide explicit applicability and implement. Reference `endpoint-typology-architecture.md` §6.3 Class A / Class B table.

- T2.1 **PII Detector** — Class A text. Applies to: chat (req+resp), embeddings (req only — input text), STT (resp transcript), image-gen (req prompt only), TTS (req input text), video-gen (req prompt only). Implementation: declare `SupportsEndpoint(t) → t != EndpointTypeJob && t != EndpointTypeBatch` AND `SupportsModality(m) → m == ModalityText`. The text/non-text split is enforced by the pipeline filter, not the hook.
- T2.2 **Keyword Filter** — same applicability as PII Detector.
- T2.3 **Content Safety** — same applicability as PII Detector.
- T2.4 **AI Guard** — Class A text. Applies to: chat (req), embeddings (req). NOT applicable to response on either (semantic classification of an embedding's float vector is meaningless; chat response classification is unsupported today).
- T2.5 **Quality Checker** — Class A text. Applies to: chat (resp). Not applicable to embeddings (no quality criteria for vectors today). Declared explicitly.
- T2.6 **Webhook Forward** — Class A passthrough. Applies to: any endpoint, any modality (admin's webhook decides). `SupportsEndpoint` returns true everywhere; `SupportsModality` returns true everywhere.
- T2.7 **Rate Limiter** — Class B. Applies everywhere.
- T2.8 **Request Size Validator** — Class B. Applies everywhere.
- T2.9 **IP Access Filter** — Class B. Applies everywhere.
- T2.10 **Rule-Pack Engine** — split into two: text-rule sub-evaluator (Class A) gated by `SupportsModality(ModalityText)`; metadata-rule sub-evaluator (Class B) gated by `SupportsEndpoint(t) → true`. Both halves share configuration; pipeline filter selects the right sub-set per endpoint.

### T3 — `Pipeline.BuildPipeline` filters at build time

- T3.1 In `packages/shared/policy/pipeline/pipeline.go`, modify `BuildPipeline` (or its equivalent constructor) to accept the `EndpointType` + `InputModality` + `OutputModality` from the calling handler.
- T3.2 The filter logic:
  ```
  hooks_to_construct = []
  for h in registry where matches_org_scope(h) and matches_stage(h):
      if !h.SupportsEndpoint(endpoint): continue
      if len(in_mod ∪ out_mod) > 0:
          if !any(h.SupportsModality(m) for m in (in_mod ∪ out_mod)): continue
      hooks_to_construct.append(h)
  ```
- T3.3 When `hooks_to_construct` ends up empty for the `response` stage of an embedding response (the canonical example), the pipeline records a stamp on the `traffic_event` row: `pipeline_skipped_reason="no_applicable_hooks_endpoint_embeddings"`. The audit row's hook decision field gets value `not_evaluated` rather than the misleading `approve`. Cross-ref FR-1.5.
- T3.4 The handler that calls `BuildPipeline` (AI Gateway `proxy.go`, Compliance Proxy `conn/`, Agent forwarder) is updated to pass the new arguments. Where the endpoint is not yet known (request-stage filter built before classifier runs), the call uses `EndpointTypeChat` for backward-compatible behaviour and reschedules the filter once classifier runs — see T3.5.
- T3.5 Two-phase build is allowed and explicitly documented: handlers MAY build a "broad" pipeline first (`EndpointType=chat`) for request-stage hooks that fire before classification, then re-evaluate the pipeline for response-stage hooks with the now-classified `EndpointType`. The cost is one extra map traversal per request — negligible at sub-ms p99. Alternative: defer all pipeline building until after classifier — chosen if implementation finds two-phase build complicates the handler too much; decision in code review.

### T4 — Prometheus metric `nexus_hook_pipeline_skipped_total`

- T4.1 New metric `nexus_hook_pipeline_skipped_total{endpoint, reason, stage}` counter, registered via `promauto` in the `pipeline` package.
- T4.2 Increment on every pipeline-build-time skip. Label values: `endpoint` (one of the EndpointType strings); `reason` ∈ {`no_applicable_hooks`, `unsupported_modality`, `passthrough_mode`}; `stage` ∈ {`request`, `response`, `streaming`}.
- T4.3 Add to the standard Prometheus scrape config (no admin action needed; metric appears automatically).

### T5 — Tests

- T5.1 `pipeline_test.go` table-driven: for each EndpointType × Modality × stage combination, assert the expected hook set is constructed.
- T5.2 Per-hook unit tests assert `SupportsEndpoint` / `SupportsModality` truth tables match the §6.3 Class A / Class B mapping. Failing a row of the truth table fails the test.
- T5.3 Integration test (handler-level): a synthetic embedding-shaped `HookInput` builds a pipeline with `BuildPipeline(EndpointTypeEmbeddings, ...)` and asserts Class-A text hooks are not constructed; Class-B hooks (rate-limit, audit) are present.
- T5.4 Backward-compat test: a chat-shaped `HookInput` (`EndpointTypeChat`, `Modality=text`) builds exactly the same pipeline today's code builds. Snapshot of hook set before E62-S1 vs after must match.
- T5.5 Audit-row assertion: after a build-time skip, `traffic_event.pipeline_skipped_reason` is set; `traffic_event.hook_decision` is `not_evaluated`, not `approve`. Existing audit-pipeline tests extended.
- T5.6 Coverage ≥95% per CLAUDE.md unit-test binding for `packages/shared/policy/hooks/core/`, `packages/shared/policy/pipeline/`.

### T6 — Doc updates

- T6.1 `docs/developers/architecture/services/ai-gateway/hook-architecture.md` body (not just the new top-of-doc note already added in E62 requirements drafting) gets a §13 or §14 section "Endpoint + Modality Awareness" that links to `endpoint-typology-architecture.md` §6 and inlines the truth table for built-in hooks.
- T6.2 The same §13 explicitly notes (per `endpoint-typology-architecture.md` §6.3 Class-label semantics): **Class A / Class B labels classify applicability (which endpoints+modalities a hook runs on), NOT operation (inspect vs modify vs annotate).** A Class-A hook MAY redact / rewrite the body during decide — PII Detector is the canonical inspect-and-redact example. The taxonomy answers "should this hook be CONSTRUCTED for this endpoint", not "what does this hook do".
- T6.3 `docs/developers/workflow/conventions.md` gets a one-line addition: "New hook implementations MUST explicitly declare `SupportsEndpoint` + `SupportsModality` — no silent inheritance from a chat-only default."
- T6.4 No change to OpenAPI (hook framework is not externally addressable via REST).

### T7 — Compliance Proxy + Agent integration

- T7.1 The CP forwarder (`packages/compliance-proxy/internal/conn/`) and Agent forwarder (`packages/agent/core/`) update their pipeline-build call to pass `EndpointType` + `Modality` from the classifier output. In E62 the classifier (built in S6) is the source; until S6 lands, both paths default to `EndpointTypeChat` (preserving today's behaviour).
- T7.2 No `interception_domain` schema change in S1 — `applicableEndpoints` field lands in S6 because it's CP-scoped.
- T7.3 macOS NE agent — pipeline-build is unchanged (still metadata-only; no content extraction; no Class-A hooks ran today and continue not to run).

---

## Acceptance Criteria

- A1: `HookInput` struct carries `EndpointType` + `InputModality` + `OutputModality` fields. Existing fields untouched.
- A2: `Hook` interface defines `SupportsEndpoint(EndpointType) bool` and `SupportsModality(Modality) bool`. Every built-in hook implements both explicitly.
- A3: `Pipeline.BuildPipeline` accepts the new args and filters at build time. Filter logic matches §T3.2 pseudocode.
- A4: For an embedding response build, Class-A text hooks (PII, Keyword, Safety, AI Guard, Quality Checker) are NOT constructed; Class-B hooks (rate limit, audit) ARE constructed. Verified by table-driven test.
- A5: For a chat response build, the constructed hook set is identical to pre-E62-S1 baseline. Verified by snapshot test.
- A6: `traffic_event.pipeline_skipped_reason` is stamped when the pipeline drops all Class-A hooks. `hook_decision` becomes `not_evaluated`. Existing pipelines (chat) continue stamping their normal decisions.
- A7: Prometheus metric `nexus_hook_pipeline_skipped_total{endpoint, reason, stage}` exists and increments on filter-skip.
- A8: Coverage ≥95% on `policy/hooks/core/` and `policy/pipeline/`.
- A9: `/smoke-gateway` continues to pass on all existing P3 / P3R / P3A / P3G phases.
- A10: Doc updates landed: `hook-architecture.md` body + `workflow/conventions.md` one-liner.

---

## Out of Scope (S1)

- Defining new Class-A hooks for image / audio / video modalities. S1 only adds the framework hook-points; concrete image-NSFW / voice-clone hooks land in E67.
- `interception_domain.applicableEndpoints` schema field — that is S6 (CP-scoped).
- Smoke phase additions — that is S5 (P3E phase for AI Gateway) and S6 (CP smoke arm).
- macOS NE TLS-bumping — out of all of E62; tracked separately.

---

## Implementation Notes

- The `EndpointType` enum is intentionally a string type, not int constants, to allow Postgres / Prometheus label values to use the same string without translation.
- The two-phase pipeline build (T3.5) is acceptable but should be measured — if the cost is observable in p99 latency, switch to single-phase post-classifier build.
- The `ChatOnly()` default helper exists for **compilation backward compatibility only** during the migration PR. Every hook should declare its applicability explicitly within the same PR; the helper is removed after.
- `Rule-Pack Engine` split (T2.10) is the most complex audit item — text-rule vs metadata-rule sub-evaluators share configuration but participate in different pipelines. Implementation note: the rule-pack config schema MAY gain a `rule_class: text|metadata` field if the split needs it; decision deferred to implementation.
