# Control Plane UI — AI Gateway: cost and cache

This document covers the cost and cache half of the AI GATEWAY sidebar section: **Quota Policies**, **Quota Overrides**, **Cache**, and **Emergency Passthrough**. The connectivity and routing half (Providers, Credentials, Credential Reliability, Routing Rules, Virtual Keys) is in [ai-gateway-routing.md](./ai-gateway-routing.md). Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

## Quota Policies

**Purpose.** Define a spending or token budget over a scope, with an action to take when the budget is exceeded.

**List page.** Columns: name, scope (with a sub-line showing the organization name, or the virtual-key type, plus the period), cost limit (USD), enforcement mode, and an enabled toggle. The list has a scope filter, an enabled filter, and a search box. Row actions are edit and delete; clicking a row opens the detail page.

**Create and detail.** Creation is grouped into four cards: Basic Info (name, description); Policy Type (scope, plus an organization picker when scope is organization, or a virtual-key type when scope is vk); Limits & Enforcement (period type, enforcement mode, cost limit, token limit, alert thresholds — default `80, 90`); and Advanced (priority, enabled). The detail page renders all fields as a key-value grid, with thresholds shown as `80% · 90%`.

**Key concepts.** `scope` is `organization`, `user`, `project`, or `vk`. For vk scope, `vkType` is `personal` or `application`. `periodType` is `daily`, `weekly`, or `monthly`. `enforcementMode` is one of `reject` (block over-budget requests), `downgrade` (route to a cheaper target), `notify-and-proceed` (alert but allow), or `track-only` (record only). `alertThresholds` is a list of integer percentages that fire alerts as usage climbs. `priority` orders policies when more than one applies.

**Where the data comes from.** `quotaPolicyApi` — `list`, `get`, `create`, `delete`; `organizationApi.list` for the org picker.

## Quota Overrides

**Purpose.** Pin a per-entity exception that overrides the matching quota policy for one specific user, virtual key, project, or organization.

**List page.** Columns: target type (badge), target (name and id), cost limit (USD), enforcement mode (or "inherit from policy"), period type (or inherit), and reason. The list has a target-type filter and a searchable target picker. Row actions are edit and delete; clicking a row opens the detail page.

**Create and detail.** Creation has two sections: Target (target type — user, vk, project, or organization — and the target itself, chosen through a searchable combobox or the organization tree) and Override Settings (cost limit, token limit, enforcement mode, period type, and a reason). Enforcement mode and period type both default to inherit, sent as unset so they fall back to the governing policy. The detail page shows the target, its organization, the limits, the enforcement and period (with their inherit fallback), and the reason.

**Key concepts.** An override differs from a policy by targeting one concrete entity rather than a scope class. Leaving enforcement mode or period type as inherit means the value comes from the policy that governs the target. The `reason` field documents why the exception exists.

**Where the data comes from.** `quotaOverrideApi` — `list`, `get`, `create`, `delete`; plus `iamApi.listUsers`, `virtualKeyApi.list`, `projectApi.list`, and `organizationApi.list` for the target pickers.

## Cache

**Purpose.** A single fleet-wide configuration page for the gateway and provider caches. Every cache setting applies across the whole fleet.

**What you see.** A sticky status strip across the top shows gateway savings and hits, provider savings and hits, freshness-rule active/total counts, and an emergency-disable dropdown. Below it are two tabs: **Gateway Cache** and **Provider Prompt Cache**.

The Gateway Cache tab renders, in order:

- **Extract cache (L1 exact-match)** — an enabled switch and a TTL in seconds (60 to 604800).
- **Semantic cache** — an embedding provider and model select with a "Run Probe" action, an enabled switch (the kill switch; disabled until a provider and model are set), a similarity `threshold` (0 to 1), an `allowCrossModel` flag, a `varyBy` selector, an `embedStrategy` selector, and a pre-warm modal that accepts JSON or CSV with a dry-run preview.
- **Freshness rules** — an `applyFreshnessRules` toggle plus a table of rules (keyword, require-question-mark, require-entity, languages, enabled) with an add-rule modal and a test box; these rules skip the cache for time-sensitive prompts.
- **Recent feedback** — a read-only table of reported bad cache hits.

The emergency-disable hooks turn the semantic cache or the extract cache off fleet-wide by re-fetching the singleton config and resubmitting it with `enabled` false, preserving the other fields.

**Key concepts.** There are three distinct cache tiers. **Extract cache** is the L1 exact-match response cache; **Semantic cache** is the vector-similarity cache — these two are separate gateway-side tiers. **Provider Prompt Cache** is a third, provider-side tier configured on its own tab. `varyBy` is `none`, `user`, `vk`, or `org`. `embedStrategy` is `last_user`, `system_plus_last_user`, `recent_turns`, `head_plus_tail`, or `full_truncated`.

**Where the data comes from.** `semanticCacheConfigApi` (`getConfig`, `saveConfig`, `runProbe`, pre-warm), `extractCacheConfigApi` (`getConfig`, `saveConfig`), `timeSensitivePatternsApi` (`list`, `create`, `update`, `delete`, `test`), `semanticFeedbackApi.listFeedback`, `analyticsApi.cacheROI`, and `systemApi.listModels`.

## Emergency Passthrough

**Purpose.** An incident-response kill switch that bypasses gateway processing stages, with a mandatory reason and an automatic expiry.

**What you see.** A banner shows whether passthrough is active, listing each enabled tier with its bypass flags and a countdown. Below it are three panels in order: **Global**, **Adapter Overrides** (a table), and **Provider Overrides** (a table).

**Controls.** Each tier has an editor with an enabled switch, three bypass switches, an expiry (a datetime-local input capped at now plus 8 hours), and a reason textarea with a live character counter. Enabling any tier pops a confirmation dialog that recaps the scope, flags, expiry, and reason. Adapter and provider overrides are added through a dialog with a type or provider selector and removed with a confirmation prompt.

**Key concepts.** The three bypass flags are **bypassHooks** (skip the compliance hooks pipeline), **bypassCache** (skip cache lookup and write), and **bypassNormalize** (skip traffic normalization). Turning on bypassNormalize forces bypassCache on as well. The kill switch is three-tier — **global**, **adapter**, and **provider** — applied from broadest to narrowest. Validation requires at least one bypass flag when a tier is enabled, a reason of at least 20 characters, and an expiry that is in the future and no more than 8 hours out; the tier auto-reverts when its expiry passes. Enabling a tier is gated on the `passthrough:emergencyEnable` action and deletion on `passthrough:write`.

**Where the data comes from.** `passthroughApi` — `getSnapshot`, `putGlobal`, `putAdapter`, `putProvider`, `deleteAdapter`, `deleteProvider`; plus `providerApi.list` for the provider picker.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'aiGateway', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/ai-gateway/quota-policies/` — Quota Policies list, create, detail, edit
- `packages/control-plane-ui/src/pages/ai-gateway/quota-overrides/` — Quota Overrides list, create, detail, edit
- `packages/control-plane-ui/src/pages/ai-gateway/cache/CachePage.tsx` — the fleet-wide cache config page
- `packages/control-plane-ui/src/pages/ai-gateway/cache/sections/` — status strip, extract cache, semantic cache, freshness rules, provider prompt cache, recent feedback cards
- `packages/control-plane-ui/src/pages/ai-gateway/cache/hooks/` — fleet-wide emergency-disable hooks for the semantic and extract caches
- `packages/control-plane-ui/src/pages/ai-gateway/passthrough/PassthroughPage.tsx` — Emergency Passthrough page
- `packages/ai-gateway/internal/execution/passthrough/` — the bypass-flag execution path
- `packages/control-plane-ui/src/api/` — `quotaPolicyApi`, `quotaOverrideApi`, `semanticCacheConfigApi`, `extractCacheConfigApi`, `timeSensitivePatternsApi`, `semanticFeedbackApi`, `passthroughApi`
- `tools/db-migrate/schema.prisma` — `QuotaPolicy`, `QuotaOverride`, `SemanticCacheConfig`, `ExtractCacheConfig` models
