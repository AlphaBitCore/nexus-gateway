# Control Plane UI — AI Gateway: routing and connectivity

This document covers the connectivity and routing half of the AI GATEWAY sidebar section: **Providers & Models**, **Credentials**, **Credential Reliability**, **Routing Rules**, and **Virtual Keys**. The cost and cache half (Quota Policies, Quota Overrides, Cache, Emergency Passthrough) is in [ai-gateway-cost-cache.md](./ai-gateway-cost-cache.md). Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

These five pages form a setup chain: register a **Provider**, attach a **Credential** to authenticate it, tune **Credential Reliability** thresholds, write **Routing Rules** that pick a provider and model per request, and issue **Virtual Keys** that authenticate callers.

## Providers & Models

**Purpose.** Register an upstream AI provider and the models it exposes to the gateway.

**List page.** Columns: name, display name, adapter type, base URL, an enabled toggle, and row actions (edit, delete). A "Create Provider" button opens the wizard; the list has a search box and an enabled/disabled status filter. The list shows providers only — models are managed inside a provider's detail page.

**Create and detail.** Creation is a five-step wizard: pick a template (or custom), fill provider fields (name, base URL, adapter type), attach a credential, add models, then review. Provider templates are loaded from static `/provider-templates/*.json` definitions. The detail page has six tabs — info, credentials, models, usage, health, cache — and the header enables/disables or deletes the provider; the models tab adds and removes models.

**Key concepts.** `adapterType` identifies the provider's wire spec (which in-tree codec talks to it). A provider with no enabled credential cannot serve traffic.

**Fetching models from the provider (OpenAI-compatible only).** On the Models step of the create-provider wizard, a "Fetch from /v1/models" button is shown for custom providers. Clicking it calls `POST /api/admin/providers/discover-models` with the base URL, adapter type, and API key entered in the earlier wizard steps. The response pre-fills the model table with the ids returned by the upstream provider's model-listing endpoint. Each row also receives a suggested model type (chat, embedding, audio, or image) derived by a best-effort heuristic from the model id; the admin can change the type before saving. The button is disabled until both a base URL and an API key (or "skip credential") are provided.

This feature is limited to providers whose adapter type speaks the OpenAI wire format — `openai`, `deepseek`, and all OpenAI-compatible adapters (Groq, Mistral, Fireworks, Together, xAI, Perplexity, HuggingFace, Moonshot). If the selected adapter does not support the standard `/v1/models` endpoint, the wizard shows an inline message that model fetch is not available for that adapter and prompts the admin to add models manually. Pricing fields are always left blank after a fetch — the `/v1/models` endpoint carries no pricing data — and must be filled manually before saving.

**Pricing not set reminder.** On a provider's models tab, any model whose input price is not configured shows a "Pricing not set" warning badge alongside its type badge. This is a reminder that cost stamping for that model will not produce a dollar amount until pricing is filled in; the badge disappears once `inputPricePerMillion` is set.

**Where the data comes from.** `providerApi` — `list`, `get`, `create`, `update`, `delete`, `getHealth`, `getModels`, `addModel`, `getAnalytics`, `getTemplates`, `getTemplateDetail`, `testExisting`, `testConnection`, `discoverModels`.

## Credentials

**Purpose.** Store an encrypted API key bound to one provider so the gateway can authenticate upstream calls.

**List page.** Columns: name, provider, an enabled toggle, pool status, reliability, expiry, last-used, and a delete action. An "Add Credential" button opens the create form; the list has a search box plus provider and status filters.

**Create and detail.** Creation collects name, provider, the API key (entered as a password field), a selection weight (1–1000), an optional expiry, and the enabled flag. The stored secret is never displayed back. The detail page has three tabs — info, reliability, history; rotation is performed by entering a new API key on the info tab. The header enables/disables or deletes the credential.

**Key concepts.** A credential moves through a rotation lifecycle — `none`, `pending_rotation`, `validating`, `rotated`, `completed`, `failed`. When several credentials back one provider, each carries a pool `status` of `active`, `retiring`, or `retired` (with a retire-at time), and a `selectionWeight` that biases which credential is chosen. The history tab shows the created / last-rotated / last-success / last-failure timeline.

**Where the data comes from.** `credentialApi` — `list`, `get`, `create`, `update`, `delete`, `circuitReset`, `probe`, `updateReliabilityOverrides`.

## Credential Reliability

**Purpose.** A fleet-wide settings page that defines the thresholds classifying credential health and driving failover.

**What you see.** This is a single settings form, not a list. It exposes seven required positive-number inputs: `authFailThreshold`, `rateLimitCooldownSeconds`, `healthyThresholdPct`, `degradedThresholdPct`, `healthMinSamples`, `healthWindowSeconds`, and `healthSustainedDegradedSeconds`. The page offers Save and Reset Defaults; client-side validation enforces that the degraded percentage is below the healthy percentage and that the healthy percentage is at most 100.

**Key concepts.** These thresholds are global; they write the `gateway.credential_reliability.config` key in system metadata. A single credential can still carry per-credential overrides, set on that credential's reliability tab rather than here.

**Where the data comes from.** `reliabilitySettingsApi` — `get`, `update`.

## Routing Rules

**Purpose.** Decide which provider and model serves a request, based on match conditions and a selection strategy.

**List page.** Columns: name (with a retry-policy badge), strategy type, priority, an enabled toggle, and edit / delete actions. A "Create Rule" button opens the form; the list has a search box plus strategy and status filters.

**Create and detail.** The form collects name, description, strategy type, priority, the enabled flag, per-strategy targets (provider plus model, with weights for load-balance and A/B-split), a fallback chain, a retry policy, and match conditions: `models`, `matchProviders`, `matchProjectIds`, `matchRequestedModelLiterals`, `matchModelTypes`, and `matchVirtualKeys`. The detail page reads and edits the same fields.

**Key concepts.** The strategy dropdown offers six options: `single` (the simple default; it also covers priority-, latency-, and cost-based ordering), `fallback` (try targets in order), `loadbalance` (weighted distribution), `conditional` (pick by request condition), `ab_split` (weighted experiment split), and `smart`. A seventh strategy type, `policy`, is not a dropdown option — it operates as the pipeline pre-narrowing stage that allow/deny-lists models and providers before a rule's strategy runs. Priority orders rules when more than one matches; lower runs first.

**Where the data comes from.** `routingApi` — `list`, `get`, `create`, `update`, `patch`, `delete`, `simulate`.

## Virtual Keys

**Purpose.** Issue scoped, client-facing keys that authenticate callers to the AI Gateway on `/v1` and constrain what they may do.

**List page.** Columns: name, project (with its organization), a status badge, expiry, an enabled toggle, and actions — approve / reject for pending keys, revoke for active keys, and delete. A "Create Virtual Key" button opens the create form; the list has a search box plus project and status filters. This page lists application-type keys only.

**Create and detail.** Creation collects name, an optional project (which binds the key to that project and its organization), a source-app label, an allowed-models list (per provider-and-model reference; an empty list means all models are allowed), a requests-per-minute rate limit, an expiry (or a never-expires flag), and the enabled flag. The secret is shown once, immediately after creation. The detail page has three tabs — info, quota, access-log; the info tab regenerates the secret (displayed afterward as a key-prefix plus masked remainder) and edits the key's scope, and the quota tab shows the rate limit.

**Key concepts.** `vkType` is `application` or `personal` — this section manages application keys; personal keys are issued by developers from their own account settings. `vkStatus` moves through `pending`, `active`, `expired`, `rejected`, and `revoked`. The create form does not link a quota policy directly; quota association is shown on the detail page's quota tab.

**Where the data comes from.** `virtualKeyApi` — `list`, `get`, `create`, `update`, `delete`, `regenerate`, `approve`, `reject`, `renew`, `revoke`.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'aiGateway', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/ai-gateway/providers/list/ProviderList.tsx` — Providers & Models list
- `packages/control-plane-ui/src/pages/ai-gateway/providers/wizard/` — provider creation wizard
- `packages/control-plane-ui/src/pages/ai-gateway/providers/detail/` — provider detail tabs
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/CredentialList.tsx` — Credentials list
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/reliability/` — fleet-wide Credential Reliability settings tab
- `packages/control-plane-ui/src/pages/_shared/settings/SettingsPageWrappers.tsx` — Credential Reliability settings wrapper
- `packages/control-plane-ui/src/pages/ai-gateway/routing/list/RoutingRuleList.tsx` — Routing Rules list
- `packages/control-plane-ui/src/pages/ai-gateway/routing/form/` — routing rule form (strategy, targets, conditions)
- `packages/control-plane-ui/src/pages/ai-gateway/virtual-keys/VirtualKeyList.tsx` — Virtual Keys list
- `packages/control-plane-ui/src/pages/ai-gateway/virtual-keys/detail/` — virtual key detail tabs
- `packages/ai-gateway/internal/routing/strategies/` — the routing strategy implementations
- `packages/control-plane-ui/src/api/` — `providerApi` (including `discoverModels`), `credentialApi`, `reliabilitySettingsApi`, `routingApi`, `virtualKeyApi`
- `packages/control-plane-ui/src/pages/ai-gateway/providers/wizard/StepModels.tsx` — Fetch from /v1/models button and model table
- `packages/control-plane-ui/src/pages/ai-gateway/providers/detail/ProviderModelsTab.tsx` — "Pricing not set" badge on the models tab
- `packages/control-plane/internal/ai/providers/handler/provider_discover.go` — CP admin handler for discover-models (IAM: provider:create)
- `docs/users/api/openapi/control-plane/providers.yaml` — OpenAPI spec for `POST /api/admin/providers/discover-models`
- `tools/db-migrate/schema/` — `Provider`, `Credential` (`providers.prisma`); `RoutingRule`, `VirtualKey` (`gateway.prisma`)
