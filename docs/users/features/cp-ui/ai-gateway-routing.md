# Control Plane UI — AI Gateway: routing and connectivity

This document covers the connectivity and routing half of the AI GATEWAY sidebar section: **Providers & Models**, **Credentials**, **Credential Reliability**, **Routing Rules**, and **Virtual Keys**. The cost and cache half (Quota Policies, Quota Overrides, Cache, Emergency Passthrough) is in [ai-gateway-cost-cache.md](./ai-gateway-cost-cache.md). Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

These five pages form a setup chain: register a **Provider**, attach a **Credential** to authenticate it, tune **Credential Reliability** thresholds, write **Routing Rules** that pick a provider and model per request, and issue **Virtual Keys** that authenticate callers.

## Providers & Models

**Purpose.** Register an upstream AI provider and the models it exposes to the gateway.

**List page.** Columns: name, display name, adapter type, base URL, an enabled toggle, and row actions (edit, delete). A "Create Provider" button opens the wizard; the list has a search box and an enabled/disabled status filter. The list shows providers only — models are managed inside a provider's detail page.

**Create and detail.** Creation is a five-step wizard: pick a template (or custom), fill provider fields (name, base URL, adapter type), attach a credential, add models, then review. Provider templates are loaded from static `/provider-templates/*.json` definitions. The detail page has six tabs — info, credentials, models, usage, health, cache — and the header enables/disables or deletes the provider; the models tab adds and removes models.

**Key concepts.** `adapterType` identifies the provider's wire spec (which in-tree codec talks to it). A provider with no enabled credential cannot serve traffic.

**Three switches take a model out of service, and all three are honoured everywhere.** A model is offered and routable only when its own enabled toggle is on, its provider's enabled toggle is on, and its status is not **Disabled**. Flipping any one of them immediately removes the model from the model catalog (`GET /v1/models` and `GET /v1/models/{model}`), from the assistant's model picker, and from routing — a request naming it is rejected as unavailable rather than dispatched upstream. The other two switches are left untouched, so the model reappears as soon as the one you flipped goes back. Turning off the provider is therefore the way to take a whole vendor out of rotation with a single toggle instead of disabling its models one by one.

The **Deprecated** and **Preview** statuses do *not* withdraw a model — they are labels, and such models stay fully callable. Only **Disabled** takes one out of service.

**Serves OpenAI Responses API.** The provider info form carries a "Serves OpenAI Responses API" setting with three choices: **Use adapter default** (the default — the gateway infers the capability from the provider's adapter type, so a real OpenAI provider serves `/v1/responses` out of the box), **Enabled**, and **Disabled (chat completions only)**. It controls whether a `/v1/responses` request routed to this provider is sent upstream in the Responses wire shape or downgraded to chat completions first. Leave it on the default unless the provider is an OpenAI-compatible endpoint that only implements `/v1/chat/completions` — set it to **Disabled** so the gateway never sends a Responses-shape body the endpoint would reject. The setting can only narrow the adapter default (it can disable a capability, not invent one); regardless of how it is set, a `/v1/responses` caller always receives a Responses-shaped reply.

**Fetching models from the provider (OpenAI-compatible only).** On the Models step of the create-provider wizard, a "Fetch from /v1/models" button is shown for custom providers. Clicking it calls `POST /api/admin/providers/discover-models` with the base URL, adapter type, and API key entered in the earlier wizard steps. The response pre-fills the model table with the ids returned by the upstream provider's model-listing endpoint. Each row also receives a suggested model type — chat, embedding, image, video, rerank, realtime, or a precise audio sub-type (`tts` for text-to-speech, `stt` for speech-to-text) — derived by a best-effort heuristic from the model id; the admin can change the type before saving. The model type is not cosmetic: routing is modality-scoped, so a model is only ever served for requests of its own modality (an image model can never be dispatched to a chat request, and `model: auto` on an image or audio endpoint only picks a model of that modality). Set the type correctly, or that model will be excluded from the endpoints it should serve. The button is disabled until both a base URL and an API key (or "skip credential") are provided.

This feature is limited to providers whose adapter type speaks the OpenAI wire format — `openai`, `deepseek`, and all OpenAI-compatible adapters (Groq, Mistral, Fireworks, Together, xAI, Perplexity, HuggingFace, Moonshot). If the selected adapter does not support the standard `/v1/models` endpoint, the wizard shows an inline message that model fetch is not available for that adapter and prompts the admin to add models manually. Pricing fields are always left blank after a fetch — the `/v1/models` endpoint carries no pricing data — and must be filled manually before saving.

**Pricing not set reminder.** On a provider's models tab, any model whose input price is not configured shows a "Pricing not set" warning badge alongside its type badge. This is a reminder that cost stamping for that model will not produce a dollar amount until pricing is filled in; the badge disappears once `inputPricePerMillion` is set.

**Per-modality pricing fields.** The model pricing section adapts to the model type. Token-priced types show the familiar input/output (and cached read/write) per-million rates. Per-unit types (per-image, per-second speech or video) are priced per 1M of their own billable unit — e.g. $0.04/image is entered as `40000` — with an inline hint explaining the conversion. A `realtime` model shows a six-field layout because realtime bills text and audio components of one response simultaneously at different rates: text input / text output / cached text read, plus audio input / audio output / cached audio read (all USD per 1M tokens; when "Cached Audio Read $/M" is not set, it falls back to the audio input rate). A realtime model needs both audio rates and both text rates filled to be servable under an enforced cost quota.

**Sync from Catalog.** A provider's models tab has a "Sync from Catalog" button that compares the provider's model rows against the built-in catalog template for the same vendor and reports the differences in three groups: models the catalog lists and this provider lacks (accepted by default), models already on this provider whose values differ from the catalog (shown field by field and accepted one row at a time, since an admin may have overridden the catalog deliberately), and models only this provider has (listed for reference — never written, never removed). Nothing is written until Apply, and only the accepted rows are written. Providers with no matching catalog template are told so rather than being offered a guess.

The button is shown only to an admin who may both create and update models, because applying a diff does both — a principal allowed only one of the two would see part of the diff committed and the rest rejected. Rows are written one request at a time, so an apply can still partly succeed: a model code already in use is rejected (codes are globally unique across providers) while its neighbours commit. When that happens the remaining rows are still attempted, the models tab refreshes so the rows that did commit are on screen, the dialog closes rather than leaving a diff that no longer matches the data, and a message reports how many rows were applied and names the ones that failed. Reopening the dialog recomputes the diff against the rows that now exist, which is how the failed rows are retried.

**Where the data comes from.** `providerApi` — `list`, `get`, `create`, `update`, `delete`, `getHealth`, `getModels`, `addModel`, `getAnalytics`, `getTemplates`, `getTemplateDetail`, `testExisting`, `testConnection`, `discoverModels`.

## Credentials

**Purpose.** Store an encrypted API key bound to one provider so the gateway can authenticate upstream calls.

**List page.** Columns: name, provider, an enabled toggle, pool status, reliability, expiry, last-used, and a delete action. An "Add Credential" button opens the create form; the list has a search box plus provider and status filters.

**Create and detail.** Creation collects name, provider, the API key (entered as a password field), a selection weight (1–1000), an optional expiry, and the enabled flag. The stored secret is never displayed back. The detail page has three tabs — info, reliability, history; rotation is performed by entering a new API key on the info tab. The header enables/disables or deletes the credential.

**Key concepts.** A credential moves through a rotation lifecycle — `none`, `pending_rotation`, `validating`, `rotated`, `completed`, `failed`. When several credentials back one provider, each carries a pool `status` of `active`, `retiring`, or `retired` (with a retire-at time), and a `selectionWeight` that biases which credential is chosen. The history tab shows the created / last-rotated / last-success / last-failure timeline.

**Permissions.** Credentials are their own permission, not part of the provider's. Editing or deleting one requires the credential update or delete permission, and an admin who may edit a provider but not its credentials sees no edit or delete action on them — on the credentials list, on the credential detail page, and on the credentials tab of a provider. This holds wherever a credential is reachable: the same permission guards the write however the admin navigated to it.

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

**Key concepts.** The strategy dropdown offers seven options: `single` (the simple default), `fallback` (try targets in order), `loadbalance` (weighted distribution), `conditional` (pick by request condition), `ab_split` (weighted experiment split), `latency` (fastest measured tier first), and `smart` (a router model picks from the models the request qualifies for, and two other models from that same pool ride along behind the pick as failover targets — which raises the upstream calls one auto-routed request may make; `retryPolicy.maxUpstreamCalls` bounds the spend). Priority orders rules when more than one matches; the HIGHER priority runs first, and every other matching rule stays behind it as a backup for when it cannot serve the request. A strategy nested inside another is refused at save time — an entry names a provider and a model directly; to back a rule up with a different strategy, author that one at a lower priority.

**Routing preview.** The rule detail page answers "if a request like this arrived, which rule wins and what would it resolve to" without sending anything upstream. Two inputs: the model the caller would name (a literal id, or `auto` to delegate), and the ENDPOINT the request would arrive on. The endpoint is the one that changes the answer most — a rule's model-type conditions, the modality filter and non-chat `auto` all key off it — so it is chosen rather than assumed; only the endpoints that actually constrain which models may serve them are offered, because the others accept any model and simulating them tells you nothing. When the preview cannot run, the gateway's own reason is shown rather than a generic failure, since this is the screen an admin is on precisely because routing is not doing what they expected.

**Where the data comes from.** `routingApi` — `list`, `get`, `create`, `update`, `patch`, `delete`, `simulate`.

## Virtual Keys

**Purpose.** Issue scoped, client-facing keys that authenticate callers to the AI Gateway on `/v1` and constrain what they may do.

**List page.** Columns: name, project (with its organization), a status badge, expiry, an enabled toggle, and actions — approve / reject for pending keys, revoke for active keys, and delete. A "Create Virtual Key" button opens the create form; the list has a search box plus project and status filters. This page lists application-type keys only.

**Create and detail.** Creation collects name, an optional project (which binds the key to that project and its organization), a source-app label, an allowed-models list (per provider-and-model reference; an empty list means all models are allowed), a requests-per-minute rate limit, an expiry (or a never-expires flag), and the enabled flag. Names must be unique among application keys (personal keys are unique per owner); leaving the name field runs an advisory duplicate check that flags a taken name inline before the rest of the form is filled in, and submission is rejected with a conflict error if the name is taken (the authoritative check). The secret is shown once, immediately after creation. The detail page has three tabs — info, quota, access-log; the info tab regenerates the secret (displayed afterward as a key-prefix plus masked remainder) and edits the key's scope, and the quota tab shows the rate limit.

**Key concepts.** `vkType` is `application` or `personal` — this section manages application keys; personal keys are issued by developers from their own account settings. `vkStatus` moves through `pending`, `active`, `expired`, `rejected`, and `revoked`. The create form does not link a quota policy directly; quota association is shown on the detail page's quota tab.

**Expiry.** The expiry rule depends on `vkType`. An application key must carry a future expiry — it cannot be open-ended; the earliest selectable date is tomorrow, and the distance is otherwise unbounded (there is no maximum window). A personal key may set an expiry or stay open-ended, and a set expiry can be cleared back to never-expires at any time. These rules apply identically on create, edit, and renew.

An expiry is a **calendar day in your own timezone**: a key set to expire on May 2 stays usable through the end of May 2 as your clock reads it, and "tomorrow" — the earliest selectable date — is tomorrow on your calendar. The date shown on the info tab, the day the picker offers, and the moment the key actually stops working therefore always agree, wherever you are. Editing a key without touching the expiry field leaves the stored expiry exactly as it was, down to the second.

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
