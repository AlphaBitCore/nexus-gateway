# E62-S5 — `/smoke-gateway` P3E (Embeddings) Phase

> Story: e62-s5
> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Requirements: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md` §FR-5
> Architecture: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` §9 (smoke harness phase fan-out + per-phase assertion template)
> Memory: `project_e62_cross_adapter_embeddings`, `feedback_cache_mandatory_all_ingress`
> Blocked by: S2 (canonical types), S3 (codecs working end-to-end), S4 (traffic_event stamping verifiable)
> Blocks: E62 close (NFR-4 — smoke must pass before E62 reports "done")

---

## User Story

As a **gateway operator running smoke checks before every deploy**, I want `/smoke-gateway` to exercise every in-scope embedding ingress × model tuple with the same rigour it applies to chat — non-streaming, batch, dimensions round-trip, cross-format consistency, traffic_event cross-check, Prometheus delta, and a reject-asymmetry negative test — so that regressions in any embedding code path surface in CI before they reach prod.

---

## Tasks

### T1 — Replace `_NON_CHAT_RE` filter with `Model.outputModalities` classification

- T1.1 In `tests/scripts/smoke-gateway.py`, remove the regex at lines 126-141 (`_NON_CHAT_RE = re.compile(r"^(text-embedding|.*\bembedding\b.*|dall-e|whisper|.*-tts|tts-)", re.I)`).
- T1.2 Replace `is_non_chat(mid)` with a per-modality classification using the `Model.outputModalities` field surfaced by `/v1/models`:
  - `outputModalities==["text"]` → chat phases (P3 / P3R / P3A / P3G).
  - `outputModalities==["embedding"]` → P3E.
  - `outputModalities==["image"]` → skip in E62 (lands in E64).
  - `outputModalities==["audio"]` → skip in E62 (lands in E63).
  - `outputModalities==["video"]` → skip in E62 (lands in E66).
- T1.3 If `/v1/models` does not yet surface `outputModalities` field (depends on S2 admin API surface), classify by model id prefix (whitelist file `tests/scripts/embedding-model-prefixes.json`) as a fallback. Switch to API-driven classification once S2 admin API exposes the new field.

### T2 — P3E phase implementation

- T2.1 New function `phase3e_embeddings(ctx, models_by_ingress)` in `tests/scripts/smoke-gateway.py`. Runs after P3 / P3R / P3A / P3G; receives the embedding-model subset.
- T2.2 For each (ingress, model) tuple, run the six arms:
  - **Arm A — non-stream basic**: POST to ingress URL with simple string input; assert 200; assert response shape matches ingress format; assert `data[0].embedding` has expected length (model's default_dimension).
  - **Arm B — dimensions round-trip**: POST with `dimensions=<half_of_max>` where the model supports it; assert response embedding length == requested. Skip with reason `model_does_not_support_dimensions` for models without support (e.g. ada-002).
  - **Arm C — batch input**: POST with N inputs (N = min(8, model.max_batch_size)); assert response carries N items in `data` array in submission order; assert each item's embedding length matches default_dimension.
  - **Arm D — traffic_event cross-check**: DB query on the most recent `traffic_event` row for the test VK; assert `endpoint_type='embeddings'`, `prompt_tokens>0`, `completion_tokens=0`, `cache_read_tokens=0`, `estimated_cost_usd>0`, `metadata.embedding.dimension` matches Arm A's response length.
  - **Arm E — Prometheus delta**: snapshot `nexus_traffic_events_total{endpoint="embeddings",provider=...,model=...}` before and after Arms A+B+C; assert delta equals number of submitted requests (excluding negative tests).
  - **Arm F — cross-ingress consistency**: for the matrix of (ingress_format, target_format) pairs in {OpenAI, Azure, Cohere, Gemini}² where ingress != target AND target supports the request:
    - Submit same input to native ingress (ingress=target) → record embedding vector `V_native`.
    - Submit same input to cross-format ingress (ingress != target) → record embedding vector `V_cross`.
    - Assert `len(V_native) == len(V_cross)` (same dimension).
    - Assert `cosine_similarity(V_native, V_cross) > 0.999` (same upstream, same model, same input → byte-identical or near-identical vectors). This catches normalize/decode divergence between codec implementations.
- T2.3 Each arm logs its outcome to the markdown report under section `## P3E — Embeddings`.

### T3 — Reject-asymmetry negative test

- T3.1 For each in-scope embedding ingress format, submit one request that VIOLATES the target's capability. Examples:
  - OpenAI ingress, `dimensions=2048`, but the only routable target (per a temporary routing-rule pin) is Cohere (fixed dimension 1024 only) → expect HTTP 400 `no_compatible_provider` with `param="dimensions"`.
  - Cohere ingress, batch of 200 inputs, but the only routable target is Cohere (max_batch_size=96) → expect HTTP 400 `no_compatible_provider` with `param="input"` or `param="batch_size"`.
- T3.2 Assertions per negative test: (a) status 400; (b) body shape matches ingress format error envelope (`error.code='no_compatible_provider'`); (c) NO traffic_event row created with `provider_id=<target>` for this request (verified by DB query); (d) Prometheus counter `nexus_traffic_events_total{endpoint="embeddings"}` did NOT increment.
- T3.3 The negative-test arm requires a temporary routing-rule pin (admin API call) before the test and a cleanup after. Use a unique VK + routing-rule combo so concurrent smoke runs don't collide.

### T4 — Cache-arm omission

- T4.1 The 2-turn cache arm that P3 / P3R / P3A / P3G run is **explicitly skipped** for P3E. Reasons:
  - Embeddings have no prompt-cache semantic (no provider-side prompt cache for embedding requests).
  - Gateway response cache may apply (if admin enabled it for an embedding endpoint), but verifying cache hits on byte-identical inputs is a different test scope (covered in E61 semantic-cache work). E62-S5 does not exercise gateway response cache for embeddings.
- T4.2 The report's P3E section explicitly states `Cache arm: skipped — embeddings have no prompt-cache semantic`. This distinguishes "skipped because not applicable" from "passed" (per FR-5.5 binding).

### T5 — Dry-run arm

- T5.1 Dry-run is **N/A** for embeddings — no `nexus.dry_run=true` body field today triggers embedding dispatch. Decision: skip the dry-run arm; report states `Dry-run arm: skipped — embedding dry-run not implemented`.
- T5.2 If a future epic implements embedding dry-run (estimate cost without upstream call), this arm fills in.

### T6 — Report extension

- T6.1 The smoke report markdown template (rendered in `render_report()`) gains a `## P3E — Embeddings` section with subheaders per (ingress, model) tuple.
- T6.2 The cross-ingress consistency matrix table extends to include embedding ingresses (today's matrix is chat-only). Each (ingress, target) cell shows pass/fail for the cross-format Arm F.
- T6.3 Negative-test results (T3) appear in a dedicated `## P3E — Reject-Asymmetry` section.
- T6.4 Total embedding requests counted in the report header summary.

### T7 — Runbook + wiring

- T7.1 Update the `/smoke-gateway` skill description in `.claude/skills/smoke-gateway/SKILL.md` to mention P3E coverage.
- T7.2 `tests/run-all.sh` continues to invoke `smoke-gateway.py --all-ingress` — P3E is automatic.
- T7.3 No new CLI flag needed by default. Add `--no-embeddings` flag for tests that genuinely don't want embedding coverage (e.g. quick smoke against a chat-only deploy).

### T7a — Smoke upstream cost policy framework

- T7a.1 New file `tests/scripts/_cost_policy.json` records per-phase upstream cost mode. P3E registers:
  ```json
  {
    "phases": {
      "P3":  { "mode": "real-upstream", "rationale": "chat tokens cost negligible" },
      "P3R": { "mode": "real-upstream" },
      "P3A": { "mode": "real-upstream" },
      "P3G": { "mode": "real-upstream" },
      "P3E": { "mode": "real-upstream", "rationale": "embedding tokens cost negligible (~$0.0002 per smoke run)" }
    },
    "default_override_flag": "--all-upstream",
    "future_phases_default": "fixture"
  }
  ```
- T7a.2 `smoke-gateway.py` reads this file at startup. Phases registered as `mode=fixture` skip real upstream by default; `--all-upstream` flips them to real-upstream regardless. P3E + chat phases stay real-upstream by default.
- T7a.3 The framework + JSON map are the load-bearing E62 deliverable. E63 (TTS / STT) adds `P3T` and `P3S` entries; E64 (image) adds `P3I` (default fixture, $0.04/image otherwise); E66 (video) adds `P3V` (default fixture, $10/video otherwise). Each new typology epic adds one JSON row, no harness rework.
- T7a.4 Report header stamps the selected mode per phase so cost auditors can correlate spend with smoke runs.

### T8 — Test infrastructure

- T8.1 The smoke test needs at least one functioning embedding provider + model per ingress format. Verify the dev-yaml / prod-config has Provider rows for OpenAI / Azure / Cohere / Gemini embeddings. If missing, add seed rows.
- T8.2 The test VK used by smoke must have access to embedding endpoints. Existing VK seed may need extension; verify at impl time.
- T8.3 The negative-test arm needs a routing-rule pin mechanism. The `/smoke-gateway` script already manages routing rules for P3 (`--routing` flag); extend to support per-model dimension caps.

---

## Acceptance Criteria

- A1: `_NON_CHAT_RE` filter is removed; embedding models are routed to P3E by `outputModalities` classification.
- A2: P3E phase runs the six arms (A non-stream, B dimensions, C batch, D traffic_event cross-check, E Prometheus delta, F cross-ingress consistency) per (ingress, model) tuple.
- A3: Reject-asymmetry negative test fires for each ingress format; passes when HTTP 400 + no traffic_event + no Prometheus increment.
- A4: Cache arm and dry-run arm are explicitly logged as `skipped — <reason>`, distinguishable from passing.
- A5: Markdown report includes a dedicated `## P3E — Embeddings` section + cross-ingress consistency matrix table extension.
- A6: P3E pass rate on the in-scope provider matrix (OpenAI + Azure + Cohere + Gemini): 100% for native ingress, 100% for cross-format where target capability matches.
- A7: P3E does not regress existing P3 / P3R / P3A / P3G phases (snapshot baseline + diff). NFR-7 (existing chat smoke unchanged) verified.
- A8: `/smoke-gateway --all-ingress` full run completes within 15 min on dev; P3E budget ≤ 2 min.
- A9: Report includes embedding test counts in the header summary.

---

## Out of Scope (S5)

- Smoke coverage for image / audio / video — E63 / E64 / E66.
- Compliance Proxy embedding smoke — S6 (different harness: `/test-compliance-proxy`).
- Agent embedding smoke — out of E62 (pf-intercept rollout).
- Performance benchmarking against upstream — smoke is correctness-only; perf is a separate concern.
- Negative-test for invalid `input_type` extensions — covered by codec unit tests, not smoke.

---

## Implementation Notes

- The cross-ingress consistency arm (F) is the most architecturally important — it catches normalize divergence between codecs. Cosine similarity > 0.999 is a tight bound but achievable: same upstream provider + same model + same input should produce byte-identical vectors. Loosen the threshold only if the upstream introduces non-determinism (none observed today).
- The negative-test routing-rule pin (T3.3) is the trickiest implementation detail. Two approaches: (a) admin API call to enable/disable a routing rule per test; (b) a special VK that's pre-pinned to a single target. (a) is more flexible but requires careful cleanup; (b) is simpler but needs multiple seed VKs. Decision in implementation.
- The Prometheus delta arm (E) uses absolute counter snapshots; running smoke in parallel would inflate the delta. The harness must serialise or use distinct VK labels for parallel runs. Existing P3 has the same constraint.
- The "skipped, not passed" report distinction (A4) is binding because it changes operator behaviour: a "passed" cache arm on embeddings would be misleading. Make this explicit.
- The macOS NE agent has no embedding content-aware smoke arm — its metadata-only path is not exercised by `/smoke-gateway` (that's the AI Gateway harness). Agent smoke is `/test-cursor-adapter` / `/test-geminiweb-adapter` style; embedding-specific agent smoke awaits pf-intercept (a future epic).
