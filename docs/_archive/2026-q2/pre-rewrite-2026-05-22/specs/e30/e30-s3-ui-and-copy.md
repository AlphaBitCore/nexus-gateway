# E30 — Story 3: Control Plane UI + i18n + interception-domains copy

## Context

Closing story. Surfaces the new `adapterType` field through the Control
Plane UI as a required **Adapter** dropdown on provider create, a mutable
dropdown on provider edit, and a read-only tag on provider detail. Bundles
the interception-domains page copy fix (FR-19) since the theme — both
Compliance Proxy and Desktop Agent consume the same adapter/interception
catalog — belongs in one delivery.

Depends on s1 (DB column) and s2 (admin API accepting / returning the field).

## User Story

**As a** platform admin using the Control Plane UI,
**I want** to pick the Adapter explicitly when I create a provider (and change
it later if I need to),
**so that** the AI Gateway selects the right wire codec regardless of the
operator-facing name I chose.

## Tasks

### 1. Admin API client types — `packages/control-plane-ui/src/`

- Provider type definitions (wherever `Provider` is declared, typically
  `src/api/services/providers.ts` or `src/types/provider.ts`): add
  `adapterType: AdapterType`, remove `type`.
- Export an `AdapterType` union string literal covering the nine values to
  match the admin API enum.

### 2. Provider create wizard / form — `packages/control-plane-ui/src/pages/providers/`

- Add a required "Adapter" `<Select>` field with nine options, labeled from
  the `providers.adapter.option.<key>` i18n key. Option values are the
  canonical lowercase strings.
- When a template is picked, auto-populate the field from `template.adapterType`.
- Include `adapterType` in the submit payload.
- Client-side validation: "Adapter is required".

### 3. Provider edit form

- Include the same Adapter dropdown; it is editable per FR-6.
- Inline helper text under the field (driven by `providers.adapter.help`)
  warns that changing the Adapter can break existing credentials, models,
  and routing rules. The warning does not block submit.

### 4. Provider detail page

- Add an "Adapter" row (or tag near the title) showing
  `t('providers:adapter.option.' + provider.adapterType)`.

### 5. i18n — new keys under `providers` namespace

Add to `src/i18n/locales/{en,zh,es}/pages.json`:

- `providers.adapter.label` — **en:** "Adapter" — **es:** "Adaptador" (zh translation in the zh locale file).
- `providers.adapter.help` — short sentence explaining the field and warning about switching.
- `providers.adapter.option.openai` through `providers.adapter.option.vertex` — nine labels. Option labels keep the canonical brand/tech wording (e.g. "OpenAI", "Anthropic", "Azure OpenAI"); they are identical across locales per `CLAUDE.md` i18n convention for technical terms.
- `providers.adapter.required` — validation error string.

Copy updated files to `public/locales/{en,zh,es}/pages.json`. Verify key
counts match across the three locales.

### 6. Interception-domains copy fix — `interceptionDomains` namespace

Update these keys in all three locales (and mirror to `public/locales/`):

- `interceptionDomains.subtitle` — **en:** "Domains and paths the Compliance Proxy and Desktop Agent intercept — shared interception catalog drives both data planes."
- `interceptionDomains.allowlistNote` — rewrite so it names both Compliance Proxy and Desktop Agent.

Pick natural zh/es translations matching the en version; keep the product-level capitalized nouns ("Compliance Proxy", "Desktop Agent") as proper nouns if the surrounding locale uses English product names elsewhere.

### 7. Vitest

- Provider form test: renders required Adapter select with nine options.
- Provider form test: submit without Adapter blocks with the validation message.
- Provider form test: selecting a template populates Adapter.
- Provider detail test: renders the Adapter label from fixture.
- Interception-domains page smoke test: subtitle includes both "Compliance Proxy" and "Desktop Agent".

## Acceptance criteria

- [ ] Provider create wizard cannot submit without an Adapter.
- [ ] Provider edit form loads the current Adapter and can change it.
- [ ] Provider detail page displays the Adapter value.
- [ ] Provider templates auto-populate Adapter when selected.
- [ ] All three locales have `providers.adapter.*` keys; key counts match.
- [ ] `public/locales/` mirrors `src/i18n/locales/`.
- [ ] Interception-domains subtitle + allowlistNote reference both Compliance Proxy and Desktop Agent in en/zh/es.
- [ ] `npm test --workspace packages/control-plane-ui` green.
