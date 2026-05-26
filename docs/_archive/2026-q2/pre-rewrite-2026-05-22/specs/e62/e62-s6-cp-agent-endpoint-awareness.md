# E62-S6 — Compliance Proxy + Agent Endpoint Awareness

> Story: e62-s6
> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Requirements: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md` §FR-9
> Architecture: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` §8.7 (three-traffic-path consistency); `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md`; `docs/developers/architecture/services/agent/agent-forwarder-architecture.md`; `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` (`NormalizedPayload.Kind` enum authority + three-source consistency invariant); `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` (macOS metadata-only constraint)
> Memory: `project_e62_cross_adapter_embeddings`, `feedback_compliance_proxy_text_first`, `feedback_agent_is_pure_forwarder`
> Blocked by: S1 (hook framework endpoint-aware), S3 (codec produces NormalizedPayload via shared/normalize)
> Blocks: E62 close (FR-9 binding — three-path consistency required)

---

## User Story

As a **compliance officer for a corporate deployment**, I want embedding traffic that transits the Compliance Proxy (or pf-bumped Agent) to be classified as `Kind=ai-embeddings`, surface the input text(s) for hook scanning (PII / Keyword / Safety on the prompt), correctly skip Class-A text hooks on the response side (the vector has no text to scan), and emit `traffic_event.endpoint_type='embeddings'` audit rows identical to what the AI Gateway produces — so that policy enforcement and audit visibility are uniform across all three Nexus traffic paths.

---

## Tasks

### T1 — Endpoint classifier package

- T1.1 New package `packages/shared/traffic/classify/`:
  ```go
  package classify

  type EndpointType string  // re-exported from packages/shared/policy/hooks/core to avoid import cycles
  type AdapterID   string

  type Rule struct {
      HostPattern    string  // exact or glob (api.openai.com, *.googleapis.com)
      Method         string  // POST, GET, ...
      PathPattern    string  // glob (/v1/embeddings, /v1*/models/*:embedContent)
      ContentType    string  // empty = any
      Endpoint       EndpointType
      AdapterID      AdapterID
  }

  type Classifier interface {
      Classify(host, method, path, contentType string) (EndpointType, AdapterID, bool)
      Register(r Rule)
  }
  ```
- T1.2 Each provider's existing adapter (`packages/shared/traffic/adapters/api/openai`, `anthropic`, `gemini`, `cohere`, `azure-openai`) registers its rules at `init()` time. Rules added in E62 cover embedding endpoints per `endpoint-typology-architecture.md` §8.7.3.
- T1.3 The Classifier is registered as a singleton in each binary's bootstrap. CP wires it in `packages/compliance-proxy/internal/conn/`. Agent wires it in `packages/agent/core/` forwarder.
- T1.4 Lookup performance: pre-build a trie on (host, method) → list of rules; per-rule path glob match. O(log N) per request.
- T1.5 Unit tests cover all rule registrations for the in-scope providers + miss cases (unrecognized host/path → returns `("",  "", false)`, caller falls back to existing default behaviour).

### T2 — `NormalizedPayload.Kind` extension

- T2.1 In `packages/shared/transport/normalize/core/`:
  - Add constant `KindAIEmbeddings = "ai-embeddings"`.
  - Add field `Inputs []string` to the `NormalizedPayload` struct.
- T2.2 The UI / hook / audit consumers that switch on `Kind` add an `ai-embeddings` branch:
  - `packages/control-plane-ui/src/pages/traffic/NormalizedPayloadView.tsx` — new renderer (T7).
  - `packages/shared/policy/pipeline/` — already endpoint-aware via S1; no Kind-switch needed beyond the existing routing.
  - `packages/nexus-hub/internal/traffic/chain/` (audit consumer) — no Kind-switch logic today; passes through.
- T2.3 Vector storage rule (per arch §8.7.1): the float array from the embedding response is **NOT** stored in `NormalizedPayload`. Only `Model`, `Usage`, and per-input metadata. This is a fixed behaviour, not configurable.

### T3 — Per-adapter `Normalize` extension

For each in-scope embedding provider adapter (`packages/shared/traffic/adapters/api/<provider>/normalize.go`):

- T3.1 The adapter's `Normalize(ctx, raw, meta)` method gains an embedding-classification branch — when `meta.Endpoint == EmbeddingsEndpoint`, dispatch to a per-adapter embedding parser.
- T3.2 Per-adapter parsers (request side, populate `Inputs []string`):
  - **OpenAI / Azure**: extract `input` field. If string → `Inputs=[s]`; if []string → `Inputs=[...]`; if []int (tokens) → `Inputs=nil` + `Warning="binary_input_token_array"` (token arrays don't decode to readable text).
  - **Cohere**: extract `texts` field ([]string) → `Inputs=[...]`.
  - **Gemini `:embedContent`**: extract `content.parts[*].text` and concatenate → `Inputs=[concat]`.
  - **Gemini `:batchEmbedContents`**: for each `requests[i]`, extract `content.parts[*].text` and concatenate → `Inputs=[concat_1, concat_2, ...]`.
- T3.3 Per-adapter parsers (response side):
  - Parse `usage.prompt_tokens` / `usage.total_tokens` (or equivalent) → `Usage.PromptTokens`, `Usage.TotalTokens`.
  - Parse `model` → `Model`.
  - Do NOT decode or store the vector array. Set `Data=nil` (no `EmbeddingDataItem` instances persisted).
- T3.4 Reuse the new `shared/normalize/codecs/<wire>_embeddings.go` Tier-1 normalizers from S3 T6 — the per-host adapter's `Normalize` delegates to them via `extract.NormalizeForAdapter` with an embedding-specific `AdapterSpecHint`.
- T3.5 Tier-1 normalizer registration: `RegisterDefaultAIBuiltins(reg)` is extended to register the four embedding wire formats. Already covered by S3 T6.5; S6 just confirms the registry wiring is reachable from CP + Agent binaries.

### T4 — Hook pipeline integration (CP + Agent forwarders)

- T4.1 In `packages/compliance-proxy/internal/conn/` after the per-host adapter Normalize step:
  - Invoke `classify.Classify(host, method, path, contentType)` → `(EndpointType, AdapterID)`.
  - Pass `EndpointType` + `Modality` (derived: text for embedding request, no output modality for vector-only response) to `pipeline.BuildPipeline` per S1 T3.4.
- T4.2 The pipeline's filter (S1) drops Class-A text hooks for the response side (no text to scan); keeps Class-A text hooks for the request side (input text is scannable).
- T4.3 Same wiring in `packages/agent/core/` forwarder for Linux + Windows (pf intercept). macOS NE agent does not perform content-extract today — classifier is still invoked on metadata only, sets `EndpointType` for audit attribution, but no Class-A hook fires regardless.
- T4.4 `interception_domain` rule schema gains optional `applicableEndpoints []EndpointType` field (FR-9.6). When set, CP only applies that rule to traffic whose classified `EndpointType` is in the list. Default empty list = all endpoints (backward compatible).

### T5 — `traffic_event` stamping in CP + Agent

- T5.1 CP and Agent audit writers (separate from AI Gateway's) stamp `traffic_event.endpoint_type` from the classifier output. Today both paths default to chat-shape stamping; replace with classifier-driven value.
- T5.2 `traffic_event.source` field distinguishes CP (`compliance-proxy`) vs Agent (`agent`) vs AI Gateway (`ai-gateway`) per existing convention.
- T5.3 `metadata.embedding.*` JSONB facts (per S4 T2): CP populates from parsed wire body where available; missing fields are omitted rather than zero-coerced (analytics distinguishes "didn't measure" from "measured zero").

### T5a — Privacy / retention (binding cross-ref)

- T5a.1 `NormalizedPayload.Inputs` is subject to the **same retention + redaction pipeline as chat `Messages[]`**. No new privacy surface. The Compliance Proxy and Agent paths already apply `hook_config.storageAction.redactSpans` to chat messages; the same rule must apply to `Inputs` automatically by virtue of the rule being keyed on Kind tag (`ai-chat` and `ai-embeddings` are both content-bearing).
- T5a.2 Reference `docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md` + `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` in the implementation review checklist. Admin who has configured chat retention does **NOT** need to reconfigure for embeddings.
- T5a.3 The spillstore-overflow path (`>256 KiB` payload → S3 reference) applies unchanged. Embedding inputs are unlikely to exceed the threshold in practice but the same machinery handles it.
- T5a.4 Test fixture: a CP smoke arm with an admin-configured `storageAction.redactSpans = ["SSN", "credit-card"]` rule sends an embedding input containing both, asserts the stored `Inputs` field has redaction applied (per chat semantics). FR-9.12 verification.

### T6 — Three-source consistency test

- T6.1 Extend the existing `packages/shared/transport/normalize/` consistency-fixture test (per `normalization-architecture.md` cross-component invariant) with embedding fixtures.
- T6.2 For each in-scope provider, drop a `testdata/embeddings/<wire>/<scenario>.json` fixture (e.g. `openai/single.json`, `openai/batch.json`, `cohere/v3-multilingual.json`, `gemini/batchembed.json`). Each fixture has request bytes + response bytes.
- T6.3 The test runs three normalize paths:
  1. `shared/normalize.Registry.Normalize(body, meta)` (direct Tier-1)
  2. `canonicalbridge.DecodeViaShared(body, format, endpoint)` (AI Gateway bridge)
  3. `shared/traffic/adapters/<provider>.Adapter.Normalize(body, meta)` (CP/Agent path)

  Assert all three produce byte-identical `NormalizedPayload` values and identical projected Usage.

### T7 — UI renderer

- T7.1 `packages/control-plane-ui/src/pages/traffic/NormalizedPayloadView.tsx` adds a renderer for `Kind=ai-embeddings`:
  - Header: model name + total tokens + dimension.
  - Body: list of input strings (truncated to ~500 chars each with expand-on-click; per FR-9.11 simple view).
  - No vector visualisation.
- T7.2 i18n keys added under `pages.traffic.normalized.embeddings.*` (EN/ZH/ES) per CLAUDE.md i18n binding. Technical terms (Token, Dimension, Provider) stay English across locales.
- T7.3 Existing `ai-chat` renderer is untouched.

### T8 — `/test-compliance-proxy` skill smoke arm

- T8.1 Extend `.claude/skills/test-compliance-proxy/SKILL.md` with an embedding through-MITM arm:
  - Send `POST https://api.openai.com/v1/embeddings` through the prod Compliance Proxy host on port `:3128` (host resolved from `tests/.env.prod`).
  - Body: `{"model":"text-embedding-3-small","input":"hello world"}`.
  - Assertions:
    - HTTP 200 from upstream (verifies MITM bump + forward worked).
    - `traffic_event` row with `source='compliance-proxy'`, `endpoint_type='embeddings'`.
    - `traffic_event_normalized.request_normalized.Kind='ai-embeddings'`.
    - `traffic_event_normalized.request_normalized.Inputs[0]='hello world'`.
    - `traffic_event_normalized.response_normalized.Usage.PromptTokens>0`.
    - `traffic_event_normalized.response_normalized.Data` length 0 OR field absent (vectors not stored).
    - Prometheus delta on `nexus_traffic_events_total{source="compliance-proxy",endpoint="embeddings"}`.
- T8.2 Also test a Cohere request through CP: `POST https://api.cohere.com/v1/embed` with `{"texts":["hello"], "model":"embed-english-v3.0", "input_type":"search_query"}`. Same assertions, different adapter.
- T8.3 Update the skill to take an `--embedding` flag (default off; turn on when testing E62 changes). When on, runs the embedding arms after the existing chat arms.

### T9 — macOS Agent constraint documentation

- T9.1 Update `agent-ne-fail-open-architecture.md` with a one-paragraph note: "Content-aware embedding hooks are not available on macOS while the agent uses `NETransparentProxyProvider`. The classifier still runs on metadata, so `traffic_event.endpoint_type='embeddings'` is correctly stamped; Class-A hooks are skipped per S1's endpoint-aware pipeline (which falls through to "no extractable content" for macOS metadata-only traffic). Promoting macOS to pf-intercept restores feature parity; tracked separately."
- T9.2 Update `agent-forwarder-architecture.md` §7 (agent reality audit) — append: "Today's macOS NE limitation extends to every non-chat endpoint typology in the same way it extends to chat. Linux + Windows pf-bumped intercept paths get full embedding support as of E62-S6."

### T10 — Tests + coverage

- T10.1 Classifier unit tests (T1.5).
- T10.2 Per-adapter `Normalize` unit tests for embedding request + response shape parsing.
- T10.3 Three-source consistency integration test (T6).
- T10.4 CP integration test (httptest fixture) — simulate a CONNECT + bump + embedding POST + assert classifier output + pipeline build args.
- T10.5 Coverage ≥95% on `shared/traffic/classify/`, `shared/traffic/adapters/api/<each-provider>/normalize.go`, `shared/transport/normalize/codecs/<wire>_embeddings.go`.

### T11 — Smoke + documentation

- T11.1 Run `/test-compliance-proxy --embedding` against prod CP before E62 closes. Report attached to the E62 close-out doc.
- T11.2 Update `endpoint-typology-architecture.md` §8.7 if any new constraint is discovered during implementation (e.g. an unforeseen Cohere field that doesn't round-trip).
- T11.3 Cross-reference E62-S6 in the SDD frontmatter of E62-S1 (which it depends on).

### T12 — Cross-path drift detector (future safeguard, not E62 scope)

Per `endpoint-typology-architecture.md` §8.7.5, the three-source consistency invariant is strong at build time but fragile in production. Codec patches that land asymmetrically across AI Gateway / CP / Agent silently produce divergent NormalizedPayload rows. Fixture tests cannot catch novel upstream shapes.

- T12.1 **NOT in E62 scope** — the drift detector requires the E65 job orchestrator. Documented here so E65's design includes a sampling job that:
  - Samples the most recent ~1h of overlapping traffic (same VK + model + request hash present in both AI Gateway and CP/Agent audit rows).
  - Hashes resulting `NormalizedPayload` from each source.
  - Alerts when mismatch rate exceeds threshold (proposed: >1%).
- T12.2 Recorded in E62-S6 SDD so the future epic owner picks it up without re-archaeology of why we want this.

---

## Acceptance Criteria

- A1: New `packages/shared/traffic/classify/` package exists with `EndpointClassifier` interface and rule registry. Rules for OpenAI / Azure / Cohere / Gemini embedding endpoints registered.
- A2: `NormalizedPayload.Kind="ai-embeddings"` constant defined. `NormalizedPayload.Inputs []string` field exists. Vector data is never stored.
- A3: Each in-scope provider adapter's `Normalize` correctly populates `Inputs` (request side) and `Usage` (response side) for embedding traffic.
- A4: CP and Agent forwarders invoke the classifier and pass `EndpointType` to `pipeline.BuildPipeline`. Hook pipeline correctly drops Class-A text hooks on embedding responses.
- A5: `interception_domain.applicableEndpoints` schema field exists (FR-9.6) and CP honours it.
- A6: CP-emitted `traffic_event` rows for embedding traffic carry `source='compliance-proxy'`, `endpoint_type='embeddings'`, `metadata.embedding.*` populated.
- A7: Three-source consistency test passes — `Registry.Normalize`, `canonicalbridge.DecodeViaShared`, and `shared/traffic/adapters/<provider>.Adapter.Normalize` produce byte-identical NormalizedPayload for embedding fixtures.
- A8: UI renderer for `Kind=ai-embeddings` shows model + tokens + input strings. i18n keys present in EN/ZH/ES.
- A9: `/test-compliance-proxy --embedding` smoke arm passes against prod CP.
- A10: macOS NE constraint documented in `agent-ne-fail-open-architecture.md` and `agent-forwarder-architecture.md`.
- A11: Coverage ≥95% on modified shared packages.

---

## Out of Scope (S6)

- macOS pf-intercept replacement of NETransparentProxyProvider — separate, large epic.
- Linux + Windows agent smoke automation for embedding — code is uniform across CP and Agent (shared adapters); coverage tracked as a follow-up smoke task, not a code task.
- Image / audio / video classifier rules — added by E63 / E64 / E66 as those endpoints land.
- Webhook deliverer for async-job completion — E65 scope.
- Tier-2 detector additions for binary endpoint formats (multipart STT, binary TTS, binary image) — added when those endpoints land.

---

## Implementation Notes

- The classifier registry pattern matches how `traffic.AdapterRegistry` already works — adapters register at `init()`. Keep the pattern symmetric.
- The `Inputs []string` field on `NormalizedPayload` is the embedding-specific cousin of `Messages[]`. UI / hook code that wants "scan all text" should iterate over both depending on Kind.
- Three-source consistency (T6) is the most important test in S6. If it fails, it means AI Gateway, CP, and Agent will produce divergent audit rows for the same upstream response — a credibility-destroying bug. CI must run this on every PR.
- The macOS NE constraint (T9) is intentional documentation, not a bug to fix in this epic. The path to fixing it (pf-based intercept on macOS) is a separate, much larger undertaking — don't put it in scope.
- The classifier's path matching uses globs, not regex. Globs cover the in-scope rules cleanly (`/v1/embeddings`, `/v1*/models/*:embedContent`); regex flexibility isn't needed today and a strict glob discourages over-engineering.
- The CP `interception_domain.applicableEndpoints` field (T4.4) is a smaller follow-up — when an admin wants to log embedding inputs but not chat (compliance scoping), this field is the lever. Default empty list = all endpoints preserves today's behaviour.
- Vector non-storage (T2.3) is a hard rule. Don't add a "verbose mode" or "for-debug-only" config to enable vector storage in CP — vectors are large, low-value, and re-derivable. If someone needs to inspect vectors, replay the embedding call in the AI Gateway side.
