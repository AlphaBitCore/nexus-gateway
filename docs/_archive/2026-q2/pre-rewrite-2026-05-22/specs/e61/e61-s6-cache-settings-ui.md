# E61-S6 — Cache Settings UI Consolidation

> Story: e61-s6
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-7
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §5, §6; `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` §10
> Blocked by: e61-s2 (policy schema), e61-s2b (Strategy enum), e61-s6b (InputStagingSelector component), e61-s6c (Embedding Settings page for the read-only chip + link target)
> Blocks: e61-s7
>
> **Architecture note (2026-05-19)**: per E61-S2 §0 two-layer split,
> the embedding-model picker MOVED OUT of this story to its own
> singleton-config page (E61-S6c, mirror of AIGuardPage). This story
> now owns the per-route policy controls ONLY (enabled / threshold /
> embed_strategy / vary_by / allow_cross_model); the embedding model
> is fleet-wide infrastructure and is configured separately.

## User Story

As a Gateway Admin opening the "Cache Settings" page for a route, I want three clearly-distinct sections — Provider Prompt Cache, Gateway Extract Cache, Gateway Semantic Cache — each with its own toggles and tooltips, plus two prominent warnings on the Semantic section about false positives and shared-VK isolation collapse, so I configure the cache deliberately and never accidentally enable a setting that bites me later.

## Tasks

### T1 — Page rename

- T1.1 `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`: rename the route entry currently labeled `promptCache` to `cacheSettings`. The path can stay (no broken links if path stays); the label and icon change.
- T1.2 `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx`: update icon mapping and i18n key.
- T1.3 i18n key migrations across `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`:
    - **Partition rule** — NOT every `pages.promptCache.*` key gets renamed. The historic `pages.promptCache.*` namespace covers both E38 Provider Prompt Cache controls (which this page still hosts in Section 1) AND the page-shell strings. Splitting:
        - **Keep under `pages.promptCache.*`** (re-used by the Provider Prompt Cache section): keys that describe upstream KV reuse — `normaliserEnabledHint`, `prefixStrategy`, `tier2Ttl`, `tier3Emit`, anything referencing `cache_control` markers / Service Tier / Gemini cachedContent. These are E38-specific copy and renaming them would force re-translation of strings that mean the same thing in the new layout.
        - **Move to `pages.cacheSettings.*`** (page-shell + the two new gateway sections): page title, top callout, section headers, every key under the new Gateway Extract Cache + Gateway Semantic Cache sections (FR-7.2 / T5.x), `nav.cacheSettings`.
        - **Migration discipline**: write a `pages.cacheSettings.providerPrompt.legacyRef` key that re-exports the E38 keys by reference (`{{$t('pages.promptCache.normaliserEnabledHint')}}`) where the new page-shell renders them. This avoids duplicating translations and keeps E38 maintenance in one place.
    - `nav.promptCache` → `nav.cacheSettings` (the sidebar entry IS being renamed, since the page label changed).
    - Run `npm run check:i18n` after the partition; it should report zero new gaps in EN/ZH/ES.
    - i18n-gap-check skill (`/i18n-gap-check`) is the verification gate; run after T1.3 lands.
- T1.4 Update `pages.promptCache.title` content from "Prompt Cache" to "Cache Settings" + matching ZH/ES translations (the technical term "cache" stays English per CLAUDE.md TS conventions; "Settings" / "设置" / "Configuración").
- T1.5 IAM impact: same `iamMW(...)` action as before. Document the no-change in the PR description per CLAUDE.md IAM-impact-review rule.
- T1.6 **URL stability** (Round-2 review correction): per T1.1 above, the **path stays the same** (`/settings/prompt-cache`) — only the route key, sidebar label, and i18n keys change. Existing admin bookmarks continue to resolve. No redirect needed. The Round-1 review tentatively proposed a redirect from `/settings/prompt-cache` → `/settings/cache` but that contradicted T1.1's path-stability decision; T1.1 prevails. If a future PR DOES rename the URL path, that PR is responsible for adding the redirect + updating release notes / docs.

### T2 — Page layout

- T2.1 Top callout (above all sections):
    > "This page controls two distinct cache layers:
    > • **Provider Prompt Cache** — reuses the model's KV-cache (saves TTFT + input cost).
    > • **Gateway Response Cache** — stores the full response (avoids the entire upstream call).
    > Both are independent; configure each based on your workload."
- T2.2 Three collapsible sections in order: Provider Prompt Cache, Gateway Extract Cache, Gateway Semantic Cache.
- T2.3 Each section has its own help icon → tooltip with the relevant architecture-doc anchor.
- T2.4 Each section's toggle can be set independently; disabling one does not affect the other.

### T3 — Section 1: Provider Prompt Cache (existing E38 controls, no behavior change)

- T3.1 Reuse the existing controls from the old Prompt Cache page (`tier2_ttl`, `tier3_emit`, `prefix_strategy`, enabled).
- T3.2 Visual styling matches the new sectioned layout.
- T3.3 Tooltip points at `prompt-cache-architecture.md`.

### T4 — Section 2: Gateway Extract Cache (new section, controls existing extract policy)

- T4.1 Fields: `enabled` (toggle), `ttl` (number, seconds), `vary_by` (select: none/user/vk/org).
- T4.2 Help: "Exact-match cache. Identical requests (byte-for-byte canonical body) return the cached response without calling the upstream."
- T4.3 No warning banners — extract cache is the lower-risk tier.

### T5 — Section 3: Gateway Semantic Cache (new — the major UI add)

> **Per-route policy ONLY**. The embedding model itself is configured
> in the dedicated Embedding Settings page (E61-S6c, mirror of
> AI-Guard's settings page). Per-route surface here NEVER picks the
> embedding model — vector spaces must match fleet-wide for cross-
> route hits to work. See E61-S2 §0 for the two-layer rationale.

- T5.1 Fields:
    - `enabled` (toggle, default off).
    - `threshold` (number, range 0.80-1.00, default 0.96, step 0.01).
    - `embed_strategy` — `<InputStagingSelector>` component (S6b).
    - `vary_by` (select: vk/org/user/none — defaults to vk).
    - `allow_cross_model` (toggle, default off).

- T5.1a Above the toggle, the page reflects **L1 fleet state** in three visual modes (P1 Round-1 review):
    - **L1 absent** (`embedding_model_id IS NULL`): yellow banner — "Semantic cache is disabled fleet-wide. Configure the **Embedding Model** under Settings → Cache Embedding before enabling it on individual routes." Per-route `enabled` toggle is **disabled** (greyed) with tooltip "Configure the embedding model first." Saving with `semantic.enabled=true` against L1 absent is rejected at the admin API (S2 T5.5 validator).
    - **L1 configured but `enabled=false`** (fleet kill switch flipped): yellow banner — "Semantic cache is configured but disabled fleet-wide (kill switch active). Enabling it on this route has no effect until the fleet switch is restored." Per-route toggle is **enabled** (admin can configure ahead of restoration) but a clear "currently inactive" pill appears next to it.
    - **L1 configured + enabled**: no banner. Per-route toggle reflects per-route state freely.
    The banner's "Settings → Cache Embedding" is always a `<LinkButton>` to the S6c page.
- T5.1c **Effective state pill** (always visible next to the per-route toggle, P1 Round-1 review): renders one of three states based on the effective-enabled cascade (`response-cache-architecture.md` §3.2):
    - `Active` (green) — L1.enabled && L1 configured && L2.enabled.
    - `Inactive — fleet disabled` (grey) — L1 conditions fail; per-route setting is moot.
    - `Inactive — route disabled` (grey) — L1 OK; admin hasn't enabled per-route.

- T5.1b Below the threshold field, show a small read-only chip:
  `Embedding: <provider> / <model> (<dim>D)` pulled from L1 via
  `/api/admin/semantic-cache/config`. Click → link to the S6c page.
  Read-only on this page — the picker lives only in S6c.
- T5.2 Two warning banners ALWAYS visible above the toggle (red border, info icon):

    **Banner 1 — False positive risk (P1):**
    > ⚠️ "Semantic cache may return responses to similar-but-different queries. Recommended for non-factual workloads (summarization, idea generation, creative writing); **avoid for factual Q&A, translation, calculations, code generation tied to specific languages/libraries.**"

    **Banner 2 — Shared VK isolation collapse (P2):**
    > ⚠️ "Semantic cache is isolated per Virtual Key (default). If multiple humans share one VK, their queries can semantically match each other's cached responses — **treat shared VKs as a single user.**"

- T5.3 When `allow_cross_model=true` is toggled on, a THIRD inline warning appears (yellow, near that toggle):
    > ⚠️ "Cross-model matches can return a cheaper model's cached response when this route routes to a premium model. Verify that workload tolerance allows this."

- T5.4 The threshold slider has a tooltip explaining: "Cosine similarity required for a match. 0.96 is the recommended default. Higher = fewer false positives but lower hit rate. Lower = more hits but more false positives."

- T5.5 *(removed — embedding picker + Test button live on the S6c
  Embedding Settings page; this per-route page only displays the
  active L1 model as a read-only chip per T5.1b.)*

### T5d — Per-route embedding-cost ceiling field (P2 / FR-6.5)

- T5d.1 Add field `embedding_cost_ceiling_usd_per_day` (optional `<NumberInput>` with placeholder "unlimited"). Help text: "Daily cap on embedding spend for this route. When exceeded, L2 auto-disables until next UTC midnight. Use to protect against misconfigured routes burning embedding cost without proportional savings. Leave empty for no cap."
- T5d.2 Render a banner above the section when L2 is currently auto-disabled by the ceiling: "L2 auto-disabled until 2026-MM-DD 00:00 UTC — daily embedding budget exceeded ($X / $Y)." Disabled state is read from a new `traffic_event` aggregate (`embedding_cost_usd_today` per route) — admin API exposes via `GET /api/admin/routing-rules/:id/cache-budget-state`.
- T5d.3 Vitest: render the banner when the API returns `auto_disabled_until_utc != null`; render the field as editable input otherwise.

### T5e — Time-sensitive freshness rules editor (P5 / FR-1.8)

> Round-1 review found that the freshness rule list was Hub-shadow-pushed but had no admin UI — admins had no self-service way to inspect / disable / add rules. Adding a dedicated section here so admins don't need Hub-config tooling.

- T5e.1 New collapsible **Section 4 — Time-Sensitive Skip Rules** at the page bottom (kept separate from the route-policy sections because the rule list is fleet-wide, not per-route).
- T5e.2 Read the active rules via `GET /api/admin/cache/time-sensitive-patterns` (already in `e61-s6-cache-admin.yaml`). Render them in a table: rule_id, keywords (chip list), require_question_mark (icon), require_entity (icon), languages, "enabled" toggle.
- T5e.3 Per-rule edit/disable goes through a NEW endpoint `PUT /api/admin/cache/time-sensitive-patterns/:id` (add to OpenAPI; IAM-gated `iam.ResourceSemanticCache.Update`). Backend updates the Hub shadow which fans out to ai-gateway instances; refresh callback in `cache/freshness/` swaps the compiled `*Detector` atomically (S1 T1.3 already covers the swap mechanism).
- T5e.4 **Test box**: "Paste a sample prompt → see which rule (if any) would fire on it." Calls a new `POST /api/admin/cache/time-sensitive-patterns/test` endpoint with `{prompt, locale}`; returns `{matched_rule_id, matched_keywords, decision}`. Quick feedback for admins tuning rules.
- T5e.5 **Add rule** modal: rule_id, keywords (multi-input), require_question_mark, require_entity, languages (multi-select). On Save, the entire list (existing + new) is written back to the Hub shadow.
- T5e.6 i18n keys under `pages.cacheSettings.timeSensitiveRules.*` in 3 locales.

### T6 — i18n keys

- T6.1 All visible strings in 3 locales (EN/ZH/ES) under `pages.cacheSettings.*`.
- T6.2 New keys (non-exhaustive): `title`, `callout`, `sectionProviderPrompt.*`, `sectionExtract.*`, `sectionSemantic.*`, `sectionSemantic.warningFalsePositive`, `sectionSemantic.warningSharedVk`, `sectionSemantic.warningCrossModel`, `sectionSemantic.thresholdHelp`, `sectionSemantic.embeddingProbe.*`.
- T6.3 i18n lint via the `i18n-gap-check` skill passes — every key present in EN/ZH/ES with non-empty translations.

### T7 — Visual + design-tokens

- T7.1 No hex / rgba literals — all colors via design tokens (CLAUDE.md TS conventions).
- T7.2 The warning banner uses the existing semantic token `--color-warning-bg` / `--color-warning-border` / `--color-warning-fg`. If those tokens don't exist (likely yes), add them in S6 to `light.css` / `dark.css`.
- T7.3 Verify via `npm run check:design-tokens` post-S6 implementation.

### T8 — Service layer + admin API

- T8.1 `packages/control-plane-ui/src/api/services/...` — extend the routing-rule service to read/write the new `response_cache_policy` shape (extract + semantic sub-objects).
- T8.2 The admin API endpoint (the existing routing-rule update endpoint) accepts the new shape per the OpenAPI in `e61-s2-cache-policy.yaml` (S2 docs).
- T8.3 `useApi` queryKey conventions per CLAUDE.md — `['admin', 'routing-rules', 'cache-policy', ruleId]`.

### T9 — Vitest tests

- T9.1 Component snapshot tests for each section.
- T9.2 Warning banner visibility tests:
    - Semantic disabled → both banners visible but section collapsed/grayed.
    - Semantic enabled → both banners visible.
    - allow_cross_model on → third warning visible.
- T9.3 Embedding-provider picker test with mocked `/admin/providers` response.
- T9.4 i18n key-presence test (asserts all keys exist in all 3 locales).

## Acceptance Criteria

- A1: The route formerly known as "Prompt Cache" renders as "Cache Settings" with the three-section layout.
- A2: All three sections are independently controllable; saving the page persists the new `response_cache_policy` shape to the routing rule.
- A3: The two semantic warning banners are visible whenever the semantic section is in view, and the cross-model warning shows when `allow_cross_model=true`.
- A4: Embedding-provider Test button calls the probe endpoint and surfaces latency + dimension.
- A5: i18n keys are complete in EN/ZH/ES.
- A6: Design-token strict check passes.

## Out of Scope (S6)

- The `<InputStagingSelector>` cross-bundle component — S6b.
- E68 negative-feedback button on the audit drawer — separate epic.
- Cache ROI dashboard L2-net-contribution chart — that's a separate UI task (could fold into S6 or stand alone; current scoping puts it in S6 if time permits).
